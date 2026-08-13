package policy

// DefaultDocument is the policy set Sluice starts with when no file is
// supplied. It is written to be read: every clause here is one an operator
// would plausibly want, and together they demonstrate all four effects.
//
// The ordering convention is priority ascending, with hard security denials
// first, residency constraints in the middle, and the broad allow last, so a
// trace reads top to bottom like an argument.
const DefaultDocument = `# Sluice default policy set.
#
# Semantics:
#   - deny always wins, regardless of priority
#   - at least one allow must match, or the request is denied
#   - constrain prunes destinations without affecting authorisation
#   - prefer reshapes the routing objectives for matching traffic
#
# Attributes: subject.*, request.*, backend.*, time.*
# Operators:  == != < <= > >= and or not in "not in" matches startswith endswith contains
# Functions:  lower upper len has ip_in_cidr split

policy "deny-unauthenticated" {
  description "Zero trust: network position is not identity."
  priority    10
  effect      deny
  when        not subject.authenticated
  message     "unauthenticated request rejected"
  tags        ["baseline", "zero-trust"]
}

policy "deny-untrusted-domain" {
  description "Only workloads from a known SPIFFE trust domain may transit."
  priority    20
  effect      deny
  when        subject.authenticated and subject.trust_domain not in ["prod.internal", "staging.internal"]
  message     "unrecognised trust domain"
  tags        ["baseline", "zero-trust"]
}

policy "payments-requires-mtls" {
  description "Bearer tokens are not sufficient for the payments API."
  priority    30
  effect      deny
  when        request.path startswith "/api/payments" and not subject.mtls
  message     "payments endpoints require mutual TLS"
  tags        ["pci"]
}

policy "admin-from-corp-only" {
  description "Administrative surface is reachable only from corporate ranges."
  priority    40
  effect      deny
  when        request.path startswith "/admin" and not ip_in_cidr(request.source_ip, "10.0.0.0/8")
  message     "administrative endpoints are restricted to the corporate network"
  tags        ["baseline"]
}

policy "pii-stays-in-eu" {
  description "GDPR: personal data from EU subjects must not leave the EU."
  priority    100
  effect      constrain
  when        request.data_class == "pii" and subject.claims["residency"] == "eu"
  require     backend.jurisdiction == "EU"
  message     "GDPR residency: EU personal data may only egress to EU regions"
  tags        ["gdpr", "residency"]
}

policy "regulated-workloads-avoid-shared-tier" {
  description "Regulated traffic may not land on burst capacity."
  priority    110
  effect      constrain
  when        request.data_class == "regulated"
  require     backend.tier == "primary"
  message     "regulated traffic is restricted to primary-tier capacity"
  tags        ["compliance"]
}

policy "interactive-traffic-favours-latency" {
  description "User-facing reads optimise for speed; the savings come from batch."
  priority    200
  effect      prefer
  when        request.path startswith "/api/v1/feed" or request.path startswith "/api/v1/search"
  prefer      { latency: 0.70, reliability: 0.20, cost: 0.07, carbon: 0.03 }
  tags        ["slo"]
}

policy "batch-traffic-favours-cost-and-carbon" {
  description "Async work has no user waiting on it, so spend the latency budget."
  priority    210
  effect      prefer
  when        request.path startswith "/batch" or subject.service == "etl"
  prefer      { cost: 0.45, carbon: 0.40, latency: 0.05, reliability: 0.10 }
  tags        ["cost", "carbon"]
}

policy "overnight-batch-chases-clean-grids" {
  description "Off-peak batch weights carbon hardest, since nothing is waiting."
  priority    220
  effect      prefer
  when        request.path startswith "/batch" and (time.hour < 6 or time.hour >= 22)
  prefer      { carbon: 0.60, cost: 0.30, latency: 0.02, reliability: 0.08 }
  tags        ["carbon"]
}

policy "allow-mesh-workloads" {
  description "Authenticated workloads from a trusted domain may transit."
  priority    900
  effect      allow
  when        subject.authenticated and subject.trust_domain in ["prod.internal", "staging.internal"]
  tags        ["baseline"]
}
`

// MustCompileDefault compiles the built-in document. It panics on failure,
// which is correct: a broken default is a build-time bug, not a runtime
// condition, and the package tests exercise this path.
func MustCompileDefault() *Set {
	s, err := Compile(DefaultDocument)
	if err != nil {
		panic("policy: built-in default document does not compile: " + err.Error())
	}
	return s
}
