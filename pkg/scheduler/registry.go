package scheduler

import (
	"fmt"
	"reflect"
	"sort"
	"sync"
)

type (
	// Object is the common interface for object handling HTTP traffic.
	Object interface {
		Close()
	}

	// StatusMeta is the fundamental struct for all objects' status.
	StatusMeta struct {
		Timestamp uint64 `yaml:"timestamp"`
	}

	// ObjectRecord is the record for booking object.
	ObjectRecord struct {
		Kind string

		// func DefaultSpec() *ObjectSpec
		DefaultSpecFunc interface{}
		// func New(spec *ObjectMeta, handlers *sync.Map, prev *Plugin) *Plugin
		NewFunc interface{}

		DependObjectKinds []string

		objectType reflect.Type
		specType   reflect.Type
	}
)

var (
	objectBook = map[string]*ObjectRecord{}
)

// ObjectKinds returns all available object kinds.
func ObjectKinds() []string {
	kinds := make([]string, 0)
	for _, or := range objectBook {
		kinds = append(kinds, or.Kind)
	}
	sort.Strings(kinds)
	return kinds
}

// Register registers objects scheduled by scheduler.
func Register(or *ObjectRecord) {
	if or.Kind == "" {
		panic("empty kind")
	}

	assert := func(x, y interface{}, err error) {
		if !reflect.DeepEqual(x, y) {
			panic(fmt.Errorf("%s: %v", or.Kind, err))
		}
	}
	assertFunc := func(name string, t reflect.Type, numIn, numOut int) {
		assert(t.Kind(), reflect.Func, fmt.Errorf("%s: not func", name))
		assert(t.NumIn(), numIn, fmt.Errorf("%s: input arguments: want %d in, got %d", name, numIn, t.NumIn()))
		assert(t.NumOut(), numOut, fmt.Errorf("%s: input arguments: want %d in, got %d", name, numOut, t.NumOut()))
	}

	orExisted, exists := objectBook[or.Kind]
	assert(exists, false, fmt.Errorf("conflict kind: %s: %#v", or.Kind, orExisted))

	// SpecFunc
	specFuncType := reflect.TypeOf(or.DefaultSpecFunc)
	assertFunc("DefaultSpecFunc", specFuncType, 0, 1)

	// Spec
	or.specType = specFuncType.Out(0)
	assert(or.specType.Kind(), reflect.Ptr, fmt.Errorf("non pointer spec"))
	assert(or.specType.Elem().Kind(), reflect.Struct,
		fmt.Errorf("non struct spec elem: %s", or.specType.Elem().Kind()))
	nameField, exists := or.specType.Elem().FieldByName("Name")
	assert(exists, true, fmt.Errorf("no Name field in spec"))
	assert(nameField.Type.Kind(), reflect.String, fmt.Errorf("Name field which is not string"))
	kindField, exists := or.specType.Elem().FieldByName("Kind")
	assert(exists, true, fmt.Errorf("no Kind field in spec"))
	assert(kindField.Type.Kind(), reflect.String, fmt.Errorf("Kind field which is not string"))
	specType := reflect.TypeOf((*Spec)(nil)).Elem()
	assert(or.specType.Implements(specType), true,
		fmt.Errorf("invalid spec: not implement scheduler.Spec"))

	// NewFunc
	newFuncType := reflect.TypeOf(or.NewFunc)
	assertFunc("NewFunc", newFuncType, 3, 1)
	assert(newFuncType.In(0), or.specType,
		fmt.Errorf("conflict NewFunc and DefaultSpecFunc: "+
			"1st input argument of NewFunc is different type from "+
			"output argument of DefaultSpecFunc"))
	assert(newFuncType.In(1), newFuncType.Out(0),
		fmt.Errorf("invalid NewFunc "+
			"2nd input argument is different type from output argument of NewFunc"))
	assert(newFuncType.In(2), reflect.TypeOf(&sync.Map{}),
		fmt.Errorf("3rd input argument of NewFunc is not %T", &sync.Map{}))

	// Object
	or.objectType = newFuncType.Out(0)
	objectType := reflect.TypeOf((*Object)(nil)).Elem()
	assert(or.objectType.Implements(objectType), true,
		fmt.Errorf("invalid object: not implement scheduler.Object"))

	// StatusFunc
	statusMethod, exists := or.objectType.MethodByName("Status")
	assert(exists, true, fmt.Errorf("no func Status"))
	// NOTE: Method always has more than one argument, the first one is the receiver.
	assertFunc("Status", statusMethod.Type, 1, 1)

	// Status
	statusType := statusMethod.Type.Out(0)
	assert(statusType.Kind(), reflect.Ptr, fmt.Errorf("non pointer Status"))
	assert(statusType.Elem().Kind(), reflect.Struct,
		fmt.Errorf("non struct Status elem: %s", statusType.Elem().Kind()))
	timestampField, exists := statusType.Elem().FieldByName("Timestamp")
	assert(exists, true, fmt.Errorf("invalid Status with no field Timestamp"))
	assert(timestampField.Type.Kind(), reflect.Uint64,
		fmt.Errorf("invalid Status with not uint64 Timestamp: %s",
			timestampField.Type.Kind()))

	// DependObjecKinds
	dependKinds := make(map[string]struct{})
	for _, dependKind := range or.DependObjectKinds {
		_, exists := dependKinds[dependKind]
		assert(exists, false, fmt.Errorf("repeated depend object kind: %s", dependKind))
		dependKinds[dependKind] = struct{}{}

		dependOr, exists := objectBook[dependKind]
		if exists {
			for _, dependKind2 := range dependOr.DependObjectKinds {
				assert(dependKind == dependKind2, false,
					fmt.Errorf("depend cycle: %s and %s", or.Kind, dependOr.Kind))
			}
		}
	}

	objectBook[or.Kind] = or
}
