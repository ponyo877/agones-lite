// Package store keeps every resource agones-lite knows about.
//
// It stands in for the Kubernetes API server and etcd. One SQLite file makes
// the state durable; a copy of everything in memory makes reads cheap. All
// writes go through the Store, so the two never disagree.
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
