// Spike module for R6 (IDEA-29).
//
// The module path sits UNDER Purser's, deliberately. Purser's connector
// contract lives in `internal/connector`, so Go will only let a package whose
// import path is inside `github.com/Einlanzerous/purser/...` implement it. That
// is worth knowing on its own — a connector cannot live in the service's own
// repo, it has to be Purser's — and it is why this spike takes the module path
// it does while keeping its files in the Catenary tree. The replace points at
// the local checkout; nothing is written into the Purser repo.
module github.com/Einlanzerous/purser/spike-r6

go 1.26

require github.com/Einlanzerous/purser v0.0.0

replace github.com/Einlanzerous/purser => /home/magos/projects/purser
