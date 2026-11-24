package store

import (
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// By names a way of sorting the objects of one kind into groups, by
// something that can be told from each object: the node a GameServer is
// on, say.
//
// The store keeps such groups current with every write, so that whoever
// wants one group need not look at every object to find it. With ten
// thousand GameServers on a hundred nodes, that is a look at a hundred
// instead of ten thousand, and it is taken every time one of them changes.
type By[T Object, G comparable] struct {
	// Name tells one way of sorting from another. Two that share a name
	// share their groups, so they must sort alike.
	Name string
	// Of says which group an object belongs to. It must go by the object
	// alone, and never change its mind about one.
	Of func(T) G
}

// Group returns one group, without copying the objects and in no particular
// order: like View, it is strictly for reading.
func Group[T Object, G comparable](t Typed[T], by By[T, G], group G) []T {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	members := groupsOf(t, by).groups[group]
	out := make([]T, 0, len(members))
	for _, obj := range members {
		out = append(out, obj.(T))
	}
	return out
}

// GroupSizes returns how many objects each group has. A group that has
// none is not in it.
func GroupSizes[T Object, G comparable](t Typed[T], by By[T, G]) map[G]int {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	groups := groupsOf(t, by).groups
	sizes := make(map[G]int, len(groups))
	for group, members := range groups {
		sizes[group] = len(members)
	}
	return sizes
}

// grouping is the groups of one By. Every write to the kind passes through
// move.
type grouping[G comparable] struct {
	of     func(Object) G
	groups map[G]map[Key]Object
}

// sorter is a grouping, whatever its groups are named by.
type sorter interface {
	move(key Key, was, now Object)
}

// move takes the object under key out of the group it was in and puts it
// into the one it belongs to now. Either object may be nil: the key was
// not there before, or is gone now.
func (g *grouping[G]) move(key Key, was, now Object) {
	if was != nil {
		group := g.of(was)
		delete(g.groups[group], key)
		if len(g.groups[group]) == 0 {
			delete(g.groups, group)
		}
	}
	if now == nil {
		return
	}
	group := g.of(now)
	if g.groups[group] == nil {
		g.groups[group] = map[Key]Object{}
	}
	g.groups[group][key] = now
}

// groupsOf returns the grouping of by, and makes it from what the store
// holds the first time it is asked for. Called with the lock held.
func groupsOf[T Object, G comparable](t Typed[T], by By[T, G]) *grouping[G] {
	s := t.store
	if made, ok := s.groupings[t.kind][by.Name]; ok {
		return made.(*grouping[G])
	}
	g := &grouping[G]{of: func(obj Object) G { return by.Of(obj.(T)) }, groups: map[G]map[Key]Object{}}
	for key, obj := range s.objects[t.kind] {
		g.move(key, nil, obj)
	}
	if s.groupings[t.kind] == nil {
		s.groupings[t.kind] = map[string]sorter{}
	}
	s.groupings[t.kind][by.Name] = g
	return g
}

// The two ways GameServers are asked for most.
var (
	byNode = By[*agonesv1.GameServer, string]{
		Name: "node",
		Of:   func(gs *agonesv1.GameServer) string { return gs.Status.NodeName },
	}
	byController = By[*agonesv1.GameServer, types.UID]{
		Name: "controller",
		Of: func(gs *agonesv1.GameServer) types.UID {
			if owner := metav1.GetControllerOfNoCopy(gs); owner != nil {
				return owner.UID
			}
			return ""
		},
	}
)

// OnNode returns the GameServers placed on a node, or with an empty name
// those that have none yet. See Group.
func OnNode(servers Typed[*agonesv1.GameServer], node string) []*agonesv1.GameServer {
	return Group(servers, byNode, node)
}

// ControlledBy returns the GameServers that owner controls, as
// metav1.IsControlledBy tells it of each. See Group.
func ControlledBy(servers Typed[*agonesv1.GameServer], owner metav1.Object) []*agonesv1.GameServer {
	return Group(servers, byController, owner.GetUID())
}
