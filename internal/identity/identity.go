// Package identity derives a zero-trust Subject from cryptographic material.
//
// Every field it produces comes from something that was verified: a client
// certificate the TLS stack already validated against a configured CA, or a
// header a trusted upstream proxy populated from a certificate it validated.
// Nothing here parses a self-asserted header into an identity, because an
// identity a caller can claim for itself is not an identity.
package identity

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/url"
	"strings"

	"github.com/saumyapatel/sluice/internal/model"
)

// SPIFFEID is a parsed SPIFFE workload identifier.
type SPIFFEID struct {
	TrustDomain string
	Path        string
	Namespace   string
	Service     string
}

// String renders the identifier back to its URI form.
func (s SPIFFEID) String() string { return "spiffe://" + s.TrustDomain + s.Path }

// ParseSPIFFE parses a SPIFFE URI.
//
// The conventional Kubernetes workload path is /ns/<namespace>/sa/<account>,
// and those segments are lifted into their own fields so a policy can write
// subject.namespace == "payments" rather than doing string surgery on the ID.
// A path in any other shape is still a valid identity; it simply has no
// namespace or service.
func ParseSPIFFE(raw string) (SPIFFEID, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return SPIFFEID{}, fmt.Errorf("identity: malformed SPIFFE URI %q: %w", raw, err)
	}
	if u.Scheme != "spiffe" {
		return SPIFFEID{}, fmt.Errorf("identity: %q is not a spiffe:// URI", raw)
	}
	if u.Host == "" {
		return SPIFFEID{}, fmt.Errorf("identity: SPIFFE URI %q has no trust domain", raw)
	}

	id := SPIFFEID{TrustDomain: u.Host, Path: u.Path}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := 0; i+1 < len(parts); i += 2 {
		switch parts[i] {
		case "ns":
			id.Namespace = parts[i+1]
		case "sa":
			id.Service = parts[i+1]
		}
	}
	return id, nil
}

// FromCertificate builds a Subject from a verified peer certificate.
func FromCertificate(cert *x509.Certificate, issuer string) model.Subject {
	sub := model.Subject{
		MTLS:          true,
		Authenticated: true,
		Issuer:        issuer,
	}
	if sub.Issuer == "" {
		sub.Issuer = cert.Issuer.String()
	}

	// A SPIFFE URI SAN is preferred; the Common Name is a fallback for
	// certificates issued before anyone adopted SPIFFE.
	for _, u := range cert.URIs {
		if u.Scheme != "spiffe" {
			continue
		}
		if id, err := ParseSPIFFE(u.String()); err == nil {
			sub.ID = id.String()
			sub.TrustDomain = id.TrustDomain
			sub.Namespace = id.Namespace
			sub.Service = id.Service
			return sub
		}
	}

	sub.ID = cert.Subject.CommonName
	if sub.ID == "" && len(cert.DNSNames) > 0 {
		sub.ID = cert.DNSNames[0]
	}
	if sub.ID == "" {
		sub.ID = "unknown-peer"
	}
	return sub
}

// FromTLS builds a Subject from a completed TLS handshake. It returns an
// anonymous Subject when no verified peer certificate is present.
func FromTLS(cs *tls.ConnectionState) model.Subject {
	if cs == nil {
		return model.Anonymous()
	}
	// VerifiedChains is populated only when the server actually validated the
	// peer against its CA pool. PeerCertificates alone would include a chain
	// the client simply offered, which proves nothing.
	if len(cs.VerifiedChains) > 0 && len(cs.VerifiedChains[0]) > 0 {
		chain := cs.VerifiedChains[0]
		issuer := ""
		if len(chain) > 1 {
			issuer = chain[len(chain)-1].Subject.String()
		}
		return FromCertificate(chain[0], issuer)
	}
	return model.Anonymous()
}

// -----------------------------------------------------------------------------
// X-Forwarded-Client-Cert
// -----------------------------------------------------------------------------

// XFCCElement is one certificate entry from an X-Forwarded-Client-Cert header.
type XFCCElement struct {
	By      string
	Hash    string
	Subject string
	URI     string
	DNS     []string
}

// ParseXFCC parses Envoy's X-Forwarded-Client-Cert header.
//
// The header is a comma-separated list of elements, each a semicolon-separated
// list of key=value pairs whose values may be quoted (and a quoted value may
// itself contain commas and semicolons — a certificate Subject usually does).
// Splitting on the separators without tracking quoting is the standard way to
// get this wrong, so the scan below is quote-aware.
//
// Trusting this header at all requires that the hop populating it is itself
// trusted and that it strips any inbound copy. Envoy does both when
// forward_client_cert_details is configured; a deployment that terminates TLS
// somewhere else must ensure the same.
func ParseXFCC(header string) []XFCCElement {
	var out []XFCCElement
	for _, raw := range splitUnquoted(header, ',') {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		var el XFCCElement
		for _, kv := range splitUnquoted(raw, ';') {
			eq := strings.IndexByte(kv, '=')
			if eq < 0 {
				continue
			}
			key := strings.ToLower(strings.TrimSpace(kv[:eq]))
			val := unquote(strings.TrimSpace(kv[eq+1:]))
			switch key {
			case "by":
				el.By = val
			case "hash":
				el.Hash = val
			case "subject":
				el.Subject = val
			case "uri":
				el.URI = val
			case "dns":
				el.DNS = append(el.DNS, val)
			}
		}
		if el.By != "" || el.Hash != "" || el.Subject != "" || el.URI != "" || len(el.DNS) > 0 {
			out = append(out, el)
		}
	}
	return out
}

func splitUnquoted(s string, sep byte) []string {
	var (
		out    []string
		start  int
		quoted bool
	)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++ // skip the escaped byte
		case '"':
			quoted = !quoted
		case sep:
			if !quoted {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
		s = strings.ReplaceAll(s, `\"`, `"`)
		s = strings.ReplaceAll(s, `\\`, `\`)
	}
	return s
}

// FromXFCC builds a Subject from an X-Forwarded-Client-Cert header.
//
// The last element is used: Envoy appends the current hop's client
// certificate, so the tail is the peer that connected to the proxy in front of
// us rather than something further upstream.
func FromXFCC(header string) model.Subject {
	elements := ParseXFCC(header)
	if len(elements) == 0 {
		return model.Anonymous()
	}
	el := elements[len(elements)-1]
	if el.URI == "" && el.Subject == "" {
		return model.Anonymous()
	}

	sub := model.Subject{MTLS: true, Authenticated: true, Issuer: el.By}
	if el.URI != "" {
		if id, err := ParseSPIFFE(el.URI); err == nil {
			sub.ID = id.String()
			sub.TrustDomain = id.TrustDomain
			sub.Namespace = id.Namespace
			sub.Service = id.Service
			return sub
		}
		sub.ID = el.URI
		return sub
	}
	sub.ID = el.Subject
	return sub
}

// TrustDomainAllowed reports whether a Subject's trust domain is in the
// allowed set. An empty set allows any domain, which is appropriate for a
// single-domain deployment and is why policy remains the primary gate.
func TrustDomainAllowed(sub model.Subject, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, d := range allowed {
		if strings.EqualFold(d, sub.TrustDomain) {
			return true
		}
	}
	return false
}
