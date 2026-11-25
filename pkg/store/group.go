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
	members := groupsOf(t, by).groups[group].all()
	out := make([]T, 0, len(members))
	for _, obj := range members {
		out = append(out, obj.(T))
	}
	return out
}

// Member returns one member of a group by its place in it, counting from
// 0, or false when the group has no such place. The place says nothing
// about the member and changes as the group does: this is for choosing one
// by chance, which takes a way to reach any one of them without a look at
// the others. Like Group, it is strictly for reading.
func Member[T Object, G comparable](t Typed[T], by By[T, G], group G, place int) (T, bool) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	members := groupsOf(t, by).groups[group].all()
	if place < 0 || place >= len(members) {
		var none T
		return none, false
	}
	return members[place].(T), true
}

// GroupSizes returns how many objects each group has. A group that has
// none is not in it.
func GroupSizes[T Object, G comparable](t Typed[T], by By[T, G]) map[G]int {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	groups := groupsOf(t, by).groups
	sizes := make(map[G]int, len(groups))
	for group, members := range groups {
		sizes[group] = len(members.objects)
	}
	return sizes
}

// grouping is the groups of one By. Every write to the kind passes through
// move.
type grouping[G comparable] struct {
	of     func(Object) G
	groups map[G]*members
}

// members is one group: its objects in a row, and the place of each in it.
// The row is what lets any one member be reached by a number, and the
// places are what lets one be taken out without a search for it. A nil
// *members is the group that has nobody.
type members struct {
	keys    []Key
	objects []Object
	place   map[Key]int
}

// all returns the row, of a group that has nobody too.
func (m *members) all() []Object {
	if m == nil {
		return nil
	}
	return m.objects
}

// put adds the object under key, at the end of the row. The key is not in
// the group at that moment: whoever changes an object takes the old one
// out first (move).
func (m *members) put(key Key, obj Object) {
	m.place[key] = len(m.keys)
	m.keys, m.objects = append(m.keys, key), append(m.objects, obj)
}

// drop takes the object under key out. The last of the row moves into its
// place, so that the row has no gap and nobody else moves.
func (m *members) drop(key Key) {
	at, there := m.place[key]
	if !there {
		return
	}
	last := len(m.keys) - 1
	m.keys[at], m.objects[at] = m.keys[last], m.objects[last]
	m.place[m.keys[at]] = at
	delete(m.place, key)
	m.objects[last] = nil // or the row would hold on to it
	m.keys, m.objects = m.keys[:last], m.objects[:last]
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
		g.leave(g.of(was), key)
	}
	if now == nil {
		return
	}
	group := g.of(now)
	if g.groups[group] == nil {
		g.groups[group] = &members{place: map[Key]int{}}
	}
	g.groups[group].put(key, now)
}

// leave takes key out of a group, and the group out when it was the last.
func (g *grouping[G]) leave(group G, key Key) {
	m := g.groups[group]
	if m == nil {
		return
	}
	m.drop(key)
	if len(m.keys) == 0 {
		delete(g.groups, group)
	}
}

// groupsOf returns the grouping of by, and makes it from what the store
// holds the first time it is asked for. Called with the lock held.
func groupsOf[T Object, G comparable](t Typed[T], by By[T, G]) *grouping[G] {
	s := t.store
	if made, ok := s.groupings[t.kind][by.Name]; ok {
		return made.(*grouping[G])
	}
	g := &grouping[G]{of: func(obj Object) G { return by.Of(obj.(T)) }, groups: map[G]*members{}}
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
