// Package config loads and validates Sluice's runtime configuration.
package config

// StripComments removes // line comments and /* block */ comments from JSON,
// preserving byte offsets by replacing comment bytes with spaces.
//
// Sluice's configuration describes routing objectives and residency
// constraints — decisions a reader needs the reasoning for, not just the
// values. Plain JSON has nowhere to put that reasoning. Stripping comments
// before decoding gives commentable configuration without taking on a YAML
// dependency, and preserving offsets means encoding/json's error positions
// still point at the right line.
func StripComments(src []byte) []byte {
	out := make([]byte, len(src))
	copy(out, src)

	const (
		normal = iota
		inString
		inLine
		inBlock
	)
	state := normal

	for i := 0; i < len(out); i++ {
		c := out[i]
		switch state {
		case normal:
			switch {
			case c == '"':
				state = inString
			case c == '/' && i+1 < len(out) && out[i+1] == '/':
				out[i], out[i+1] = ' ', ' '
				i++
				state = inLine
			case c == '/' && i+1 < len(out) && out[i+1] == '*':
				out[i], out[i+1] = ' ', ' '
				i++
				state = inBlock
			}

		case inString:
			if c == '\\' {
				// Skip the escaped byte so an escaped quote does not look
				// like the end of the string.
				i++
				continue
			}
			if c == '"' {
				state = normal
			}

		case inLine:
			if c == '\n' {
				state = normal
				continue
			}
			out[i] = ' '

		case inBlock:
			if c == '*' && i+1 < len(out) && out[i+1] == '/' {
				out[i], out[i+1] = ' ', ' '
				i++
				state = normal
				continue
			}
			// Newlines are preserved so reported line numbers stay correct.
			if c != '\n' {
				out[i] = ' '
			}
		}
	}
	return out
}
