package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"

	"github.com/hexdecteam/easegateway/pkg/common"
	"github.com/hexdecteam/easegateway/pkg/logger"

	"github.com/dgraph-io/badger"
)

type OperationAppended func(seq uint64, newOperation *Operation) (error, OperationFailureType)

const (
	maxSeqKey = "maxSeqKey"
)

// TODO: Replace badger with readable text (self-implement maybe).

// opLog's methods prefixed by underscore(_) can't be invoked by other functions
type opLog struct {
	sync.RWMutex
	db                         *badger.DB
	operationAppendedCallbacks *common.NamedCallbackSet
	path                       string
}

func NewOPLog(path string) (*opLog, error) {
	err := os.MkdirAll(path, 0700)
	if err != nil {
		return nil, err
	}

	new := false

	fp, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fp.Close()

	_, err = fp.Readdirnames(1)
	if err != nil {
		if err == io.EOF {
			new = true
		} else {
			return nil, err
		}
	}

	opt := badger.DefaultOptions
	opt.Dir = path
	opt.ValueDir = path
	opt.SyncWrites = true // consistence is more important than performance

	logger.Debugf("[operation logs path: %s]", path)

	db, err := badger.Open(opt)
	if err != nil {
		return nil, err
	}

	op := &opLog{
		db:                         db,
		operationAppendedCallbacks: common.NewNamedCallbackSet(),
		path:                       path,
	}

	if new { // init max sequence to prevent fake read error
		txn := op.db.NewTransaction(true)
		defer txn.Discard()

		op._locklessWriteMaxSeq(txn, 0)

		err = txn.Commit(nil)
		if err != nil {
			logger.Errorf("[BUG: commit initial transaction failed: %v]", err)
		}
	}

	go op._cleanup()

	return op, nil
}

func (op *opLog) Path() string {
	return op.path
}

// Size returns the size of oplog files in bytes.
func (op *opLog) Size() uint64 {
	lsm, vlog := op.db.Size()
	return uint64(lsm + vlog)
}

func (op *opLog) MaxSeq() uint64 {
	op.RLock()
	defer op.RUnlock()
	txn := op.db.NewTransaction(false)
	defer txn.Discard()
	return op._locklessReadMaxSeq(txn)
}

func (op *opLog) MinSeq() uint64 {
	op.RLock()
	defer op.RUnlock()
	txn := op.db.NewTransaction(false)
	defer txn.Discard()
	return op._locklessReadMinSeq(txn)
}

func (op *opLog) Append(startSeq uint64, operations []*Operation) (error, ClusterErrorType) {
	if len(operations) == 0 {
		return nil, NoneClusterError
	}

	op.Lock()
	defer op.Unlock()

	txn := op.db.NewTransaction(true)
	defer txn.Discard()

	ms := op._locklessReadMaxSeq(txn)

	if startSeq == 0 {
		return fmt.Errorf("invalid sequential operation"), InternalServerError
	} else if startSeq > ms+1 {
		return fmt.Errorf("invalid sequential operation"), OperationInvalidSeqError
	} else if startSeq < ms+1 {
		return fmt.Errorf("operation conflict"), OperationSeqConflictError
	}

	for idx, operation := range operations {
		switch {
		case operation.ContentCreatePlugin != nil:
		case operation.ContentUpdatePlugin != nil:
		case operation.ContentDeletePlugin != nil:
		case operation.ContentCreatePipeline != nil:
		case operation.ContentUpdatePipeline != nil:
		case operation.ContentDeletePipeline != nil:
		default:
			return fmt.Errorf("operation content is empty"), OperationInvalidContentError
		}

		opBuff, err := json.Marshal(operation)
		if err != nil {
			logger.Errorf("[BUG: marshal operation (sequence=%d) %#v failed: %v]",
				startSeq+uint64(idx), operation, err)
			return fmt.Errorf("marshal operation (sequence=%d) %#v failed: %v",
				startSeq+uint64(idx), operation, err), OperationInvalidContentError
		}

		err = txn.Set([]byte(fmt.Sprintf("%d", startSeq+uint64(idx))), opBuff)
		if err != nil {
			logger.Errorf("[set operation (sequence=%d) to badger failed: %v]", startSeq+uint64(idx), err)
			return fmt.Errorf("set operation (sequence=%d) to badger failed: %v",
				startSeq+uint64(idx), err), InternalServerError
		}

		_, err = op._locklessIncreaseMaxSeq(txn)
		if err != nil {
			logger.Errorf("[update max operation sequence failed: %v]", err)
			return fmt.Errorf("update max operation sequence failed: %v", err), InternalServerError
		}

		for _, cb := range op.operationAppendedCallbacks.GetCallbacks() {
			err, failureType := cb.Callback().(OperationAppended)(startSeq+uint64(idx), operation)
			if err != nil {
				logger.Errorf("[operation (sequence=%d) failed (failure type=%d): %v]",
					startSeq+uint64(idx), failureType, err)

				clusterErrType := InternalServerError

				switch failureType {
				case NoneOperationFailure:
					logger.Errorf("[BUG: operation callback returns error without " +
						"a certain failure type]")
					clusterErrType = NoneOperationFailure
				case OperationGeneralFailure:
					clusterErrType = OperationGeneralFailureError
				case OperationTargetNotFoundFailure:
					clusterErrType = OperationTargetNotFoundFailureError
				case OperationNotAcceptableFailure:
					clusterErrType = OperationNotAcceptableFailureError
				case OperationConflictFailure:
					clusterErrType = OperationConflictFailureError
				case OperationUnknownFailure:
					clusterErrType = OperationUnknownFailureError
				}

				return fmt.Errorf("operation (sequence=%d) failed: %v", startSeq+uint64(idx), err), clusterErrType
			}
		}
	}

	err := txn.Commit(nil)
	if err != nil {
		logger.Errorf("[BUG: commit transaction failed: %v]", err)
		return fmt.Errorf("commit transaction failed: %v", err), NoneClusterError
	}

	return nil, NoneClusterError
}

// retrieve logs whose sequence are [startSeq, MIN(max-sequence, startSeq + countLimit - 1)]
func (op *opLog) Retrieve(startSeq, countLimit uint64) ([]*Operation, error, ClusterErrorType) {
	// NOTICE: We never change recorded content, so it's unnecessary to use RLock.
	txn := op.db.NewTransaction(false)
	defer txn.Discard()

	ms := op._locklessReadMaxSeq(txn)

	var ret []*Operation

	if startSeq == 0 {
		return nil, fmt.Errorf("invalid begin sequential operation"), InternalServerError
	} else if startSeq > ms {
		return ret, nil, NoneClusterError
	}

	for idx := uint64(0); idx < countLimit && startSeq+uint64(idx) <= op._locklessReadMaxSeq(txn); idx++ {
		item, err := txn.Get([]byte(fmt.Sprintf("%d", startSeq+uint64(idx))))
		if err != nil {
			logger.Errorf("[get operation (sequence=%d) from badger failed: %v]",
				startSeq+uint64(idx), err)
			return nil, fmt.Errorf("get operation (sequence=%d) from badger failed: %v",
				startSeq+uint64(idx), err), InternalServerError
		}

		opBuff, err := item.Value()
		if err != nil || opBuff == nil || len(opBuff) == 0 {
			logger.Errorf("[BUG: get empty operation (sequence=%d) from badger]",
				startSeq+uint64(idx))
			return nil, fmt.Errorf("get empty operation (sequence=%d) from badger",
					startSeq+uint64(idx)),
				InternalServerError
		}

		operation := new(Operation)
		err = json.Unmarshal(opBuff, operation)
		if err != nil {
			logger.Errorf("[BUG: unmarshal operation (sequence=%d) %#v failed: %v]",
				startSeq+uint64(idx), opBuff, err)
			return nil, fmt.Errorf("unmarshal operation (sequence=%d) %#v failed: %v",
				startSeq+uint64(idx), opBuff, err), InternalServerError
		}

		ret = append(ret, operation)
	}

	return ret, nil, NoneClusterError
}

func (op *opLog) Close() error {
	return op.db.Close()
}

func (op *opLog) AddOPLogAppendedCallback(name string, callback OperationAppended, priority string) {
	op.Lock()
	op.operationAppendedCallbacks = common.AddCallback(op.operationAppendedCallbacks, name, callback, priority)
	op.Unlock()
}

func (op *opLog) DeleteOPLogAppendedCallback(name string) {
	op.Lock()
	op.operationAppendedCallbacks = common.DeleteCallback(op.operationAppendedCallbacks, name)
	op.Unlock()
}

////

// _locklessReadMinSeq is designed to be invoked by locked methods of opLog
func (op *opLog) _locklessReadMinSeq(txn *badger.Txn) uint64 {
	// FIXME(shengdong) implement this when we need oplog shrank
	if op._locklessReadMaxSeq(txn) > 0 {
		return 1
	}
	return 0
}

// _locklessReadMaxSeq is designed to be invoked by locked methods of opLog
func (op *opLog) _locklessReadMaxSeq(txn *badger.Txn) uint64 {
	item, err := txn.Get([]byte(maxSeqKey))
	if err != nil {
		logger.Errorf("[get max sequence from badger failed: %v]", err)
		return 0
	}

	maxSeq, err := item.Value()
	if err != nil || maxSeq == nil || len(maxSeq) == 0 {
		// at the beginning, it is not a bug to get empty value.
		maxSeq = []byte("0")
	}

	ms, err := strconv.ParseUint(string(maxSeq), 0, 64)
	if err != nil {
		logger.Errorf("[BUG: parse max sequence %s failed: %s]", string(maxSeq), err)
		return 0
	}

	return ms
}

// _locklessIncreaseMaxSeq is designed to be invoked by locked methods of opLog
func (op *opLog) _locklessIncreaseMaxSeq(txn *badger.Txn) (uint64, error) {
	ms := op._locklessReadMaxSeq(txn)
	ms++
	return op._locklessWriteMaxSeq(txn, ms)
}

func (op *opLog) _locklessWriteMaxSeq(txn *badger.Txn, ms uint64) (uint64, error) {
	err := txn.Set([]byte(maxSeqKey), []byte(fmt.Sprintf("%d", ms)))
	if err != nil {
		logger.Errorf("[set max sequence to badger failed: %v]", err)
		return 0, err
	}

	return ms, nil
}

func (op *opLog) _cleanup() {
	// TODO: clean very old values
}