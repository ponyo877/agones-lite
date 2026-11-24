// Package fleets decides which GameServerSets a Fleet has and how big each
// one is.
//
// Start with plan.go: Plan finds the active set, the one made from the
// Fleet's current template, and says what to create, resize and delete.
// strategy.go decides the sizes while sets from an earlier template are
// still around, and rollingupdate.go is the longer of its two strategies.
// status.go adds the sets' status up into the Fleet's.
//
// The functions in those files only look at their arguments. controller.go
// reads the store and carries out what Plan decided.
//
// Upstream counterparts, both in pkg/fleets: controller.go (syncFleet and
// the functions it calls) and controller_rollingupdatefix.go. The alpha
// RollingUpdateSurge feature in controller_rollingupdatesurge.go is left out.
package fleets
