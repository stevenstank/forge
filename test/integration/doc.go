//go:build integration

// Package integration holds Forge's privileged integration tests.
//
// Everything here requires root and a Linux kernel with namespaces and the
// cgroups v2 unified hierarchy enabled, so it sits behind the "integration"
// build tag and never runs under `make test` (SSOT §7, §13.8). Run it with
// `sudo -E make test-integration`.
//
// Every test must release the kernel resources it creates via t.Cleanup, even
// when the test fails (PRD NFR-5).
package integration
