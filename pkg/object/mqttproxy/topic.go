/*
 * Copyright (c) 2017, MegaEase
 * All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package mqttproxy

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/megaease/easegress/pkg/logger"
)

type topicMapFunc func(mqttTopic string) (topic string, headers map[string]string, err error)

// TopicManager to manage topic subscribe and unsubscribe in MQTT
type TopicManager struct {
	sync.RWMutex
	root     *topicNode
	levelMgr *topicLevelManager
}

type topicLevelManager struct {
	sync.RWMutex
	data map[string]*topicLevels
}

type topicLevels struct {
	levels []string
	valid  bool
}

func newTopicManager() *TopicManager {
	return &TopicManager{
		root:     newNode(),
		levelMgr: newTopicLevelManager(),
	}
}

func newTopicLevelManager() *topicLevelManager {
	return &topicLevelManager{
		data: make(map[string]*topicLevels),
	}
}

func validTopic(levels []string) bool {
	for i, level := range levels {
		if strings.ContainsRune(level, '+') && len(level) != 1 {
			return false
		}
		if strings.ContainsRune(level, '#') {
			if len(level) != 1 || i != len(levels)-1 {
				return false
			}
		}
	}
	return len(levels) > 0
}

func (mgr *TopicManager) getLevels(topic string) ([]string, error) {
	return mgr.levelMgr.get(topic)
}

func (mgr *TopicManager) subscribe(topics []string, qoss []byte, clientID string) error {
	mgr.Lock()
	defer mgr.Unlock()

	for i, t := range topics {
		if err := mgr.insert(t, qoss[i], clientID); err != nil {
			return err
		}
	}
	return nil
}

func (mgr *TopicManager) unsubscribe(topics []string, clientID string) error {
	mgr.Lock()
	defer mgr.Unlock()

	for _, t := range topics {
		if err := mgr.remove(t, clientID); err != nil {
			return err
		}
	}
	return nil
}

// findSubscribers is used to find all clients that subscribe a certain topic directly or use wildcard.
// for example, topic "loc/device/event" will find clients that subscribe topic "+/+/+" or "loc/+/event" or "loc/device/event"
// so, clients subscribe topics that contain or not contain wildcard, and this function will find all subscirbed topics that match
// the given topic.
func (mgr *TopicManager) findSubscribers(topic string) (map[string]byte, error) {
	mgr.RLock()
	defer mgr.RUnlock()

	levels, err := mgr.getLevels(topic)
	if err != nil {
		return nil, err
	}
	ans := make(map[string]byte)

	currentLevelNodes := []*topicNode{mgr.root}
	for _, topicLevel := range levels {
		nextLevelNodes := []*topicNode{}
		for _, node := range currentLevelNodes {
			for nodeLevel, nextNode := range node.nodes {
				if nodeLevel == "#" {
					nextNode.addClients(ans)

				} else if nodeLevel == "+" || nodeLevel == topicLevel {
					nextLevelNodes = append(nextLevelNodes, nextNode)
				}
			}
		}
		currentLevelNodes = nextLevelNodes
		if len(currentLevelNodes) == 0 {
			return ans, nil
		}
	}
	for _, n := range currentLevelNodes {
		n.addClients(ans)
	}
	return ans, nil
}

func (mgr *TopicManager) insert(topic string, qos byte, clientID string) error {
	levels, err := mgr.getLevels(topic)
	if err != nil {
		return err
	}
	node := mgr.root
	var nextNode *topicNode
	var ok bool
	for _, l := range levels {
		nextNode, ok = node.nodes[l]
		if !ok {
			nextNode = newNode()
			node.nodes[l] = nextNode
		}
		node = nextNode
	}
	node.clients[clientID] = qos
	return nil
}

func (mgr *TopicManager) remove(topic string, clientID string) error {
	levels, err := mgr.getLevels(topic)
	if err != nil {
		return err
	}
	node := mgr.root
	var nextNode *topicNode
	var ok bool
	prevNodes := []*topicNode{}
	for _, l := range levels {
		if nextNode, ok = node.nodes[l]; !ok {
			return nil
		}
		prevNodes = append(prevNodes, node)
		node = nextNode
	}
	delete(node.clients, clientID)

	// clear memory
	for i := len(prevNodes) - 1; i >= 0; i-- {
		node = prevNodes[i].nodes[levels[i]]
		if len(node.clients) == 0 && len(node.nodes) == 0 {
			delete(prevNodes[i].nodes, levels[i])
		} else {
			return nil
		}
	}
	return nil
}

func (t *topicLevelManager) get(topic string) ([]string, error) {
	t.RLock()
	defer t.RUnlock()
	if val, ok := t.data[topic]; ok {
		if val.valid {
			return val.levels, nil
		}
		return nil, fmt.Errorf("topic %v is invalid", topic)
	}

	levels := strings.Split(topic, "/")
	valid := validTopic(levels)
	go t.put(topic, levels, valid)
	if valid {
		return levels, nil
	}
	return nil, fmt.Errorf("topic %v is invalid", topic)
}

func (t *topicLevelManager) put(topic string, levels []string, valid bool) {
	t.Lock()
	defer t.Unlock()
	t.data[topic] = &topicLevels{levels, valid}
}

type topicNode struct {
	// client with their qos
	clients map[string]byte
	nodes   map[string]*topicNode
}

func newNode() *topicNode {
	return &topicNode{
		clients: make(map[string]byte),
		nodes:   make(map[string]*topicNode),
	}
}

func (node *topicNode) addClients(ans map[string]byte) {
	for client, qos := range node.clients {
		ans[client] = qos
	}
}

func getPolicyRoute(routes []*PolicyRe) map[string]*regexp.Regexp {
	ans := make(map[string]*regexp.Regexp)
	for _, route := range routes {
		r, err := regexp.Compile(route.MatchExpr)
		if err != nil {
			logger.Errorf("topicMapper policy <%s> match expr <%s> compile failed: %v", route.Name, route.MatchExpr, err)
		} else {
			ans[route.Name] = r
		}
	}
	return ans
}

type topicRouteType map[string][]*regexp.Regexp

func getTopicRoute(p *Policy) topicRouteType {
	m := make(map[string][]*regexp.Regexp)
	for _, route := range p.Route {
		for _, expr := range route.Exprs {
			r, err := regexp.Compile(expr)
			if err != nil {
				logger.Errorf("topicMapper policy <%s> topic route expr <%s> compile failed: %v", p.Name, expr, err)
			} else {
				m[route.Topic] = append(m[route.Topic], r)
			}
		}
	}
	return m
}

func getPolicyMap(ps []*Policy) map[string]*Policy {
	ans := make(map[string]*Policy)
	for _, p := range ps {
		ans[p.Name] = p
	}
	return ans
}

func getTopicMapFunc(topicMapper *TopicMapper) topicMapFunc {
	if topicMapper == nil {
		return nil
	}
	policyMap := getPolicyMap(topicMapper.Policies)
	policyRoute := getPolicyRoute(topicMapper.Route)
	topicRoutes := make(map[string]topicRouteType)
	for _, p := range topicMapper.Policies {
		topicRoutes[p.Name] = getTopicRoute(p)
	}

	getPolicy := func(level string) *Policy {
		for name, r := range policyRoute {
			if r.MatchString(level) {
				return policyMap[name]
			}
		}
		return nil
	}

	getTopic := func(level string, p *Policy) (string, error) {
		topicRoute := topicRoutes[p.Name]
		for _, route := range p.Route {
			regexps := topicRoute[route.Topic]
			for _, regexp := range regexps {
				if regexp.MatchString(level) {
					return route.Topic, nil
				}
			}
		}
		return "", fmt.Errorf("no match topic for level <%s> with policy <%s>", level, p.Name)
	}

	f := func(mqttTopic string) (string, map[string]string, error) {
		levels := strings.Split(mqttTopic, "/")
		if levels[0] == "" {
			levels = levels[1:]
		}
		if len(levels) <= topicMapper.MatchIndex {
			return "", nil, fmt.Errorf("levels in mqtt topic <%s> is less than policy match index <%d>", mqttTopic, topicMapper.MatchIndex)
		}
		p := getPolicy(levels[topicMapper.MatchIndex])
		if p == nil {
			return "", nil, fmt.Errorf("no policy match mqtt topic <%s>", mqttTopic)
		}
		if len(levels) <= p.TopicIndex {
			return "", nil, fmt.Errorf("levels in mqtt topic <%s> is less then policy <%s> topic index <%d>", mqttTopic, p.Name, p.TopicIndex)
		}
		topic, err := getTopic(levels[p.TopicIndex], p)
		if err != nil {
			return "", nil, fmt.Errorf("mqttTopic %s get backend massage queue topic failed: %v", mqttTopic, err)
		}
		headers := make(map[string]string)
		for k, v := range p.Headers {
			if k >= len(levels) {
				continue
			}
			headers[v] = levels[k]
		}
		return topic, headers, nil
	}
	return f
}