package kstreams

import (
	"errors"

	"github.com/birdayz/kstreams/sdk"
	"github.com/twmb/franz-go/pkg/kgo"
	"golang.org/x/exp/slices"
)

type Nexter[K, V any] interface {
	AddNext(InputProcessor[K, V])
}

type TopologyBuilder struct {
	processors map[string]*TopologyProcessor
	stores     map[string]*TopologyStore

	// Key = TopicNode
	sources map[string]*TopologySource
	sinks   map[string]*TopologySink

	// processorToParent map[string]string

	childNodes map[string][]string
}

func (tb *TopologyBuilder) Build() *Topology {
	return &Topology{}
}

// PartitionGroup is a sub-graph of nodes that must be co-partitioned as they depend on each other.
type PartitionGroup struct {
	sourceTopics   []string
	processorNames []string
	storeNames     []string
}

// Contains reports whether v is present in s.
func ContainsAny[E comparable](s []E, v []E) bool {
	for _, item := range s {
		for _, check := range v {
			if item == check {
				return true
			}
		}
	}

	return false
}

func mergeIteration(pgs []*PartitionGroup) (altered []*PartitionGroup, done bool) {
	var a, b int

	var dirty bool
outer:
	for i, pg := range pgs {

		for d, otherPg := range pgs {
			if i == d {
				continue
			}
			if ContainsAny(otherPg.sourceTopics, pg.sourceTopics) || ContainsAny(otherPg.processorNames, pg.processorNames) || ContainsAny(otherPg.storeNames, pg.storeNames) {
				a = i
				b = d
				dirty = true
				break outer
			}
		}
	}

	// Clean, return
	if !dirty {
		return pgs, true
	}

	// "Sort" so it's deterministic.
	if a < b {
		a, b = b, a
	}

	// Merge b into a.
	pgA := pgs[a]
	pgB := pgs[b]

	pgA.sourceTopics = slices.Compact(append(pgA.sourceTopics, pgB.sourceTopics...))
	pgA.processorNames = slices.Compact(append(pgA.processorNames, pgB.processorNames...))
	pgA.storeNames = slices.Compact(append(pgA.storeNames, pgB.storeNames...))

	pgs = slices.Delete(pgs, b, b+1)

	return pgs, false
}

// If there is any overlap in the input partition groups, they are merged together.
func mergePartitionGroups(pgs []*PartitionGroup) []*PartitionGroup {
	finished := false
	for !finished {
		pgs, finished = mergeIteration(pgs)
	}

	return pgs
}

func NewTopologyBuilder() *TopologyBuilder {
	return &TopologyBuilder{
		processors: map[string]*TopologyProcessor{},
		stores:     map[string]*TopologyStore{},
		sources:    map[string]*TopologySource{},
		childNodes: map[string][]string{},
		sinks:      map[string]*TopologySink{},
	}
}

type TopologyStore struct {
	Name  string
	Build sdk.StoreBuilder
}

type TopologySink struct {
	Name    string
	Builder func(*kgo.Client) Flusher
}

type TopologyProcessor struct {
	Name           string
	Build          func() BaseProcessor
	ChildNodeNames []string
	AddChildFunc   func(parent any, child any, childName string) // TODO - possible to do w/o parent ?
	StoreNames     []string
}

type TopologySource struct {
	Name           string
	Build          func() RecordProcessor
	ChildNodeNames []string
	AddChildFunc   func(parent any, child any, childName string) // TODO - possible to do w/o parent ?
}

func MustAddSource[K, V any](t *TopologyBuilder, name string, topic string, keyDeserializer sdk.Deserializer[K], valueDeserializer sdk.Deserializer[V]) {
	must(AddSource(t, name, topic, keyDeserializer, valueDeserializer))
}

func AddSource[K, V any](t *TopologyBuilder, name string, topic string, keyDeserializer sdk.Deserializer[K], valueDeserializer sdk.Deserializer[V]) error {
	topoSource := &TopologySource{
		Name: name,
		Build: func() RecordProcessor {
			return &SourceNode[K, V]{KeyDeserializer: keyDeserializer, ValueDeserializer: valueDeserializer}
		},
		AddChildFunc: func(parent, child any, childName string) {
			parentNode, ok := parent.(*SourceNode[K, V])
			if !ok {
				panic("type error")
			}

			childNode, ok := child.(InputProcessor[K, V])
			if !ok {
				panic("type error")

			}

			parentNode.AddNext(childNode)

		},
		ChildNodeNames: []string{},
	}

	if _, found := t.processors[name]; found {
		return ErrNodeAlreadyExists
	}

	t.sources[name] = topoSource

	return nil
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func MustAddSink[K, V any](t *TopologyBuilder, name, topic string, keySerializer sdk.Serializer[K], valueSerializer sdk.Serializer[V]) {
	must(AddSink(t, name, topic, keySerializer, valueSerializer))
}

func AddSink[K, V any](t *TopologyBuilder, name, topic string, keySerializer sdk.Serializer[K], valueSerializer sdk.Serializer[V]) error {
	topoSink := &TopologySink{
		Name: name,
		Builder: func(client *kgo.Client) Flusher {
			return NewSinkNode(client, topic, keySerializer, valueSerializer)
		},
	}

	// t.processors[name] = topoProcessor
	t.sinks[name] = topoSink

	return nil
}

func MustAddProcessor[Kin, Vin, Kout, Vout any](t *TopologyBuilder, p ProcessorBuilder[Kin, Vin, Kout, Vout], name string, stores ...string) {
	must(AddProcessor(t, p, name, stores...))
}

func AddProcessor[Kin, Vin, Kout, Vout any](t *TopologyBuilder, p ProcessorBuilder[Kin, Vin, Kout, Vout], name string, stores ...string) error {
	topoProcessor := &TopologyProcessor{
		Name: name,
		Build: func() BaseProcessor {
			px := &ProcessorNode[Kin, Vin, Kout, Vout]{
				userProcessor: p(),
				outputs:       map[string]InputProcessor[Kout, Vout]{},
			}
			return px
		},
		ChildNodeNames: []string{},
		StoreNames:     stores,
	}

	// TODO validate store names, existence ?

	topoProcessor.AddChildFunc = func(parent any, child any, childName string) {
		// TODO: try to detect these already when building the topology.
		parentNode, ok := parent.(*ProcessorNode[Kin, Vin, Kout, Vout])
		if !ok {
			panic("type error")
		}

		// TODO: try to detect these already when building the topology.
		childNode, ok := child.(InputProcessor[Kout, Vout])
		if !ok {
			panic("type error")
		}

		parentNode.outputs[childName] = childNode
	}

	if _, found := t.processors[name]; found {
		return ErrNodeAlreadyExists
	}

	t.processors[name] = topoProcessor

	for _, store := range stores {
		if _, ok := t.stores[store]; !ok {
			return errors.New("store not found")
		}
	}
	return nil
}

func MustSetParent(t *TopologyBuilder, parent, child string) {
	must(SetParent(t, parent, child))
}

func SetParent(t *TopologyBuilder, parent, child string) error {
	parentNode, ok := t.processors[parent]
	if !ok {
		return ErrNodeNotFound
	}

	// TODO validate child exists.

	parentNode.ChildNodeNames = append(parentNode.ChildNodeNames, child)

	// t.processorToParent[child] = parent

	_, ok = t.childNodes[parent]
	if !ok {
		t.childNodes[parent] = []string{}
	}

	t.childNodes[parent] = append(t.childNodes[parent], child)

	return nil

}

var ErrNodeAlreadyExists = errors.New("node exists already")
var ErrNodeNotFound = errors.New("node not found")
var ErrInternal = errors.New("internal")

func RegisterSource[K, V any](t *TopologyBuilder, name string, topic string, keyDeserializer sdk.Deserializer[K], valueDeserializer sdk.Deserializer[V]) {
	MustAddSource(t, name, topic, keyDeserializer, valueDeserializer)
}

func RegisterSink[K, V any](t *TopologyBuilder, name string, topic string, keySerializer sdk.Serializer[K], valueSerializer sdk.Serializer[V], parent string) {
	MustAddSink(t, name, topic, keySerializer, valueSerializer)
	MustSetParent(t, parent, name)
}

func RegisterProcessor[Kin, Vin, Kout, Vout any](t *TopologyBuilder, p ProcessorBuilder[Kin, Vin, Kout, Vout], name, parent string, stores ...string) {
	MustAddProcessor(t, p, name, stores...)
	MustSetParent(t, parent, name)
}