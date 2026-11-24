package store

import agonesv1 "agones.dev/agones/pkg/apis/agones/v1"

// grouping keeps the objects of one kind sorted into groups, by something
// that can be told from each object: the node a GameServer is on, say. It
// is kept current with every write, so that whoever wants one group need
// not look at every object to find it. With ten thousand GameServers on a
// hundred nodes, that is a look at a hundred instead of ten thousand, and
// it is taken every time one of them changes.
type grouping struct {
	of     func(Object) string
	groups map[string]map[Key]Object
}

// move takes the object under key out of the group it was in and puts it
// into the one it belongs to now. Either object may be nil: the key was
// not there before, or is gone now.
func (g *grouping) move(key Key, was, now Object) {
	if was != nil {
		group := g.groups[g.of(was)]
		delete(group, key)
		if len(group) == 0 {
			delete(g.groups, g.of(was))
		}
	}
	if now == nil {
		return
	}
	if g.groups[g.of(now)] == nil {
		g.groups[g.of(now)] = map[Key]Object{}
	}
	g.groups[g.of(now)][key] = now
}

// grouped returns one group of a grouping, without copying the objects and
// in no particular order: like View, it is strictly for reading. The
// grouping is made the first time it is asked for, under the name given,
// and whoever asks under that name afterwards shares it.
func (s *Store) grouped(kind Kind, name string, of func(Object) string, group string) []Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, made := s.groupings[kind][name]
	if !made {
		g = &grouping{of: of, groups: map[string]map[Key]Object{}}
		for key, obj := range s.objects[kind] {
			g.move(key, nil, obj)
		}
		if s.groupings[kind] == nil {
			s.groupings[kind] = map[string]*grouping{}
		}
		s.groupings[kind][name] = g
	}
	out := make([]Object, 0, len(g.groups[group]))
	for _, obj := range g.groups[group] {
		out = append(out, obj)
	}
	return out
}

// OnNode returns the GameServers placed on a node, or with an empty name
// those that have none yet. Like View it does not copy: read them, never
// change them. Unlike View it does not look at every GameServer to find
// them.
func OnNode(servers Typed[*agonesv1.GameServer], node string) []*agonesv1.GameServer {
	return typed[*agonesv1.GameServer](servers.store.grouped(servers.kind, "node", nodeOf, node))
}

func nodeOf(obj Object) string {
	return obj.(*agonesv1.GameServer).Status.NodeName
}
