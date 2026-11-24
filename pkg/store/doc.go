// Package store keeps every resource agones-lite knows about.
//
// It stands in for the Kubernetes API server and etcd. One SQLite file makes
// the state durable; a copy of everything in memory makes reads cheap. All
// writes go through the Store, so the two never disagree, except that
// memory may be a moment ahead of the file: save.go says when, and who
// waits for the file to catch up. The file holds JSON, which keeps a time
// to the second, so the times the Store sets itself are whole seconds; a
// caller that stores a finer one finds it rounded down at once.
//
//   - store.go: reads and writes
//   - save.go: how a write gets into the file, at once or behind
//   - sqlite.go: the file
//   - watch.go: telling those who follow the changes
//   - group.go: finding some objects without a look at all of them
//   - typed.go: all of the above with the Go types filled in
//
// Three ideas are borrowed from Kubernetes:
//
//   - Every write gets the next number from a single counter, and the number
//     is stored in the object's resourceVersion.
//   - Update only succeeds when the caller holds the latest resourceVersion.
//     That is the optimistic lock controllers rely on.
//   - Watch reports every change, so controllers react instead of polling.
//
// Upstream counterpart: none. Agones gets all of this from client-go.
package store
