package route53

import (
	"fmt"
	"slices"
	"strings"
)

// normalize returns a DNS name as Route 53 stores it: lowercase, fully
// qualified, and with characters other than letters, digits, hyphens, and
// underscores (a wildcard's * among them) escaped as \ooo octal codes. It
// accepts names with or without the final dot and with escapes already in
// place.
func normalize(name string) (string, error) {
	name = strings.TrimSuffix(name, ".")
	if name == "" {
		return "", fmt.Errorf("the domain name is empty")
	}

	var b strings.Builder
	label, length := 0, 0
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '.':
			if label == 0 {
				return "", fmt.Errorf("the domain name %q has an empty label", name)
			}

			b.WriteByte('.')
			label = 0
			length++
			continue
		case c == '\\' && i+3 < len(name) && isOctal(name[i+1:i+4]):
			b.WriteString(name[i : i+4])
			i += 3
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c + 'a' - 'A')
		case c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_':
			b.WriteByte(c)
		case c > ' ' && c < 0x7f:
			fmt.Fprintf(&b, "\\%03o", c)
		default:
			return "", fmt.Errorf("the domain name %q has an invalid character", name)
		}

		label++
		length++
		if label > 63 {
			return "", fmt.Errorf("the domain name %q has a label longer than 63 characters", name)
		}
	}

	if length > 254 {
		return "", fmt.Errorf("the domain name %q is longer than 255 characters", name)
	}

	return b.String() + ".", nil
}

func isOctal(s string) bool {
	return len(s) == 3 && s[0] >= '0' && s[0] <= '3' && s[1] >= '0' && s[1] <= '7' && s[2] >= '0' && s[2] <= '7'
}

// reversed returns a name with its labels reversed, which orders names as
// Route 53 lists them.
func reversed(name string) string {
	labels := strings.Split(strings.TrimSuffix(name, "."), ".")
	slices.Reverse(labels)
	return strings.Join(labels, ".")
}

// within reports whether name is zone or one of its subdomains.
func within(name, zone string) bool {
	return name == zone || strings.HasSuffix(name, "."+zone)
}
