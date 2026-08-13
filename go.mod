module github.com/saumyapatel/sluice

go 1.26

// Pinned to a floor, not a preference. govulncheck reports five reachable
// standard-library vulnerabilities in 1.26.5, two of them on paths Sluice
// exercises on every request — asn1.Unmarshal via tls.LoadX509KeyPair when the
// data plane loads its certificates, and net/http on every probe and proxied
// call. All are fixed in 1.26.6.
//
// The go command downloads this toolchain automatically, so a contributor on an
// older release cannot accidentally build a vulnerable binary.
toolchain go1.26.6
