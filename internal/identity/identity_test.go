package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/url"
	"testing"
	"time"

	"github.com/Saumya-patel-31/sluice/internal/model"
)

func TestParseSPIFFE(t *testing.T) {
	id, err := ParseSPIFFE("spiffe://prod.internal/ns/payments/sa/checkout")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if id.TrustDomain != "prod.internal" || id.Namespace != "payments" || id.Service != "checkout" {
		t.Errorf("parsed %+v", id)
	}
	if got := id.String(); got != "spiffe://prod.internal/ns/payments/sa/checkout" {
		t.Errorf("round trip produced %q", got)
	}

	// A non-Kubernetes path shape is still a valid identity; it just has no
	// namespace or service to lift out.
	other, err := ParseSPIFFE("spiffe://example.org/workload/api")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if other.TrustDomain != "example.org" || other.Namespace != "" {
		t.Errorf("parsed %+v", other)
	}

	for _, bad := range []string{
		"https://prod.internal/ns/a/sa/b", // wrong scheme
		"spiffe:///ns/a/sa/b",             // no trust domain
		"not a uri at all ::::",
	} {
		if _, err := ParseSPIFFE(bad); err == nil {
			t.Errorf("ParseSPIFFE(%q) should have failed", bad)
		}
	}
}

func TestParseXFCC(t *testing.T) {
	// A realistic header: quoted Subject containing both a comma and an equals
	// sign, which is exactly where a naive split on the separators breaks.
	header := `By=spiffe://cluster.local/ns/istio-system/sa/ingress;Hash=abc123;` +
		`Subject="CN=checkout,OU=payments,O=Example Ltd";URI=spiffe://prod.internal/ns/payments/sa/checkout`

	els := ParseXFCC(header)
	if len(els) != 1 {
		t.Fatalf("want 1 element, got %d: %+v", len(els), els)
	}
	el := els[0]
	if el.Subject != "CN=checkout,OU=payments,O=Example Ltd" {
		t.Errorf("Subject = %q — the quoted value was split", el.Subject)
	}
	if el.URI != "spiffe://prod.internal/ns/payments/sa/checkout" {
		t.Errorf("URI = %q", el.URI)
	}
	if el.Hash != "abc123" {
		t.Errorf("Hash = %q", el.Hash)
	}
}

func TestParseXFCCMultipleHops(t *testing.T) {
	header := `By=spiffe://a/edge;URI=spiffe://a/ns/x/sa/outer,` +
		`By=spiffe://a/mesh;URI=spiffe://a/ns/y/sa/inner`

	els := ParseXFCC(header)
	if len(els) != 2 {
		t.Fatalf("want 2 elements, got %d", len(els))
	}

	// The last element is the peer that connected to the proxy in front of us,
	// not something further upstream.
	sub := FromXFCC(header)
	if sub.Service != "inner" {
		t.Errorf("want the nearest hop's identity, got %q", sub.ID)
	}
	if !sub.MTLS || !sub.Authenticated {
		t.Error("a certificate-derived identity is authenticated and mTLS")
	}
}

func TestFromXFCCEdgeCases(t *testing.T) {
	if sub := FromXFCC(""); sub.Authenticated {
		t.Error("an empty header is anonymous")
	}
	if sub := FromXFCC("Hash=abc;By=spiffe://a/b"); sub.Authenticated {
		t.Error("an element with no URI or Subject proves no identity")
	}

	// A Subject-only element (a certificate predating SPIFFE) still yields an
	// identity, just without trust-domain structure.
	sub := FromXFCC(`By=spiffe://a/edge;Subject="CN=legacy-service"`)
	if !sub.Authenticated || sub.ID != "CN=legacy-service" {
		t.Errorf("legacy certificate produced %+v", sub)
	}

	// A URI that is not a SPIFFE ID is retained verbatim rather than dropped.
	other := FromXFCC(`By=spiffe://a/edge;URI=https://example.com/id`)
	if other.ID != "https://example.com/id" {
		t.Errorf("non-SPIFFE URI produced %q", other.ID)
	}
}

func TestFromTLSRequiresVerifiedChain(t *testing.T) {
	cert := makeCert(t, "spiffe://prod.internal/ns/api/sa/gateway")

	// PeerCertificates alone is a chain the client merely offered. Only
	// VerifiedChains means the server validated it against its CA pool, and
	// only that may produce an authenticated identity.
	unverified := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	if sub := FromTLS(unverified); sub.Authenticated {
		t.Fatal("an unverified peer chain must not authenticate")
	}

	verified := &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{cert},
		VerifiedChains:   [][]*x509.Certificate{{cert}},
	}
	sub := FromTLS(verified)
	if !sub.Authenticated || !sub.MTLS {
		t.Fatal("a verified chain should authenticate")
	}
	if sub.Namespace != "api" || sub.Service != "gateway" {
		t.Errorf("SPIFFE fields not lifted: %+v", sub)
	}

	if sub := FromTLS(nil); sub.Authenticated {
		t.Error("no TLS state is anonymous")
	}
}

func TestFromCertificateFallsBackToCommonName(t *testing.T) {
	cert := makeCert(t, "") // no URI SAN
	sub := FromCertificate(cert, "CN=test-ca")
	if sub.ID != "legacy.example.com" {
		t.Errorf("want the Common Name, got %q", sub.ID)
	}
	if sub.Issuer != "CN=test-ca" {
		t.Errorf("issuer = %q", sub.Issuer)
	}
}

func TestTrustDomainAllowed(t *testing.T) {
	sub := model.Subject{TrustDomain: "prod.internal"}

	if !TrustDomainAllowed(sub, nil) {
		t.Error("an empty allow-list permits any domain; policy remains the gate")
	}
	if !TrustDomainAllowed(sub, []string{"staging.internal", "PROD.INTERNAL"}) {
		t.Error("matching should be case-insensitive")
	}
	if TrustDomainAllowed(sub, []string{"other.internal"}) {
		t.Error("an unlisted domain must be refused")
	}
}

/* ── Helpers ────────────────────────────────────────────────────────────── */

func makeCert(t *testing.T, spiffeURI string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "legacy.example.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	if spiffeURI != "" {
		u, err := url.Parse(spiffeURI)
		if err != nil {
			t.Fatalf("uri: %v", err)
		}
		tmpl.URIs = []*url.URL{u}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return cert
}
