package proxy

import "regexp"

var domainPattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

func ValidDomainName(name string) bool {
	return len(name) <= 253 && domainPattern.MatchString(name)
}

// DomainIndexKey identifies the globally unique host registered with a gateway version.
func DomainIndexKey(service, name string) string {
	return "aws/" + service + "/domain/" + name
}
