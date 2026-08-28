package image

import (
	"errors"
	"fmt"
	"strings"
)

// Registry and repository defaults, which exist because the familiar short
// forms are abbreviations rather than a different syntax.
const (
	// defaultRegistry is where a reference with no registry component lives.
	defaultRegistry = "docker.io"

	// defaultRegistryHost is the API endpoint docker.io is served from. The
	// canonical name and the host that answers /v2/ differ only for Docker Hub,
	// which is why Registry holds the name and Host() resolves it.
	defaultRegistryHost = "registry-1.docker.io"

	// officialRepositoryPrefix is prepended to single-component repositories on
	// Docker Hub, so "alpine" means "library/alpine".
	officialRepositoryPrefix = "library/"

	// defaultTag is the tag a reference with neither tag nor digest carries.
	defaultTag = "latest"
)

// maxTagLength is the Distribution Spec's limit on a tag.
const maxTagLength = 128

// Reference is an image reference, parsed into the parts a registry request
// needs.
//
// Exactly one of Tag and Digest is set. A reference that names both — the
// legal-but-unusual "alpine:3.20@sha256:…" — keeps the digest and drops the
// tag, because the digest is the part that actually identifies content and the
// tag is then only a hint about where it came from.
type Reference struct {
	// Registry is the canonical registry name, such as "docker.io" or
	// "ghcr.io". It is a name, not necessarily the host serving the API; see
	// Host.
	Registry string

	// Repository is the full path within the registry, such as
	// "library/alpine". The "library/" prefix of an official Docker Hub image
	// is present here even when the user did not type it.
	Repository string

	// Tag is the mutable name of the image, such as "3.20". Empty when Digest
	// is set.
	Tag string

	// Digest is the immutable content address of the image, such as
	// "sha256:…". Empty when Tag is set.
	Digest string

	// Original is exactly what the caller passed, kept so error messages can
	// quote the user's own words rather than Forge's expansion of them.
	Original string
}

// ParseReference parses an image reference in any of the forms Forge accepts:
//
//	alpine                              → docker.io/library/alpine:latest
//	alpine:3.20                         → docker.io/library/alpine:3.20
//	library/alpine:3.20                 → docker.io/library/alpine:3.20
//	docker.io/library/alpine:3.20       → docker.io/library/alpine:3.20
//	ghcr.io/org/image:latest            → ghcr.io/org/image:latest
//	localhost:5000/img@sha256:<64 hex>  → localhost:5000/img@sha256:<64 hex>
//
// It performs no I/O and is the only place the naming conventions live.
//
// The one genuinely ambiguous case is whether a reference's first component is
// a registry or the first directory of a repository: "localhost:5000/img" and
// "org/image" have the same shape. The rule, which is the same one Docker and
// containerd use, is that a first component is a registry if it contains a "."
// or a ":", or is exactly "localhost". It is a heuristic, but it is the
// heuristic every other tool applies, so a reference that works elsewhere works
// here.
func ParseReference(s string) (Reference, error) {
	original := s
	if s == "" {
		return Reference{}, fmt.Errorf("%w: empty", ErrInvalidReference)
	}
	if strings.ContainsAny(s, " \t\n\x00") {
		return Reference{}, fmt.Errorf("%w: %q contains whitespace or a NUL byte", ErrInvalidReference, original)
	}

	// The digest is split off first: it is the only part that may contain a
	// ":", so removing it makes the tag split below unambiguous.
	name := s
	var digest string
	if at := strings.LastIndex(name, "@"); at >= 0 {
		digest = name[at+1:]
		name = name[:at]
		if err := validateDigest(digest); err != nil {
			return Reference{}, fmt.Errorf("%w: %q: %w", ErrInvalidReference, original, err)
		}
	}

	// A ":" after the last "/" is a tag separator; before it, it is a registry
	// port. That is the whole of why the split is written this way.
	var tag string
	if colon := strings.LastIndex(name, ":"); colon > strings.LastIndex(name, "/") {
		tag = name[colon+1:]
		name = name[:colon]
		if tag == "" {
			// The user typed a colon and then nothing. Defaulting to :latest
			// here would silently run an image they did not ask for.
			return Reference{}, fmt.Errorf("%w: %q has a tag separator but no tag", ErrInvalidReference, original)
		}
	}

	registry, repository := splitRegistry(name)

	if registry == defaultRegistry && !strings.Contains(repository, "/") {
		repository = officialRepositoryPrefix + repository
	}

	if err := validateRepository(repository, original); err != nil {
		return Reference{}, err
	}

	switch {
	case digest != "":
		// A digest identifies content; a tag alongside it is redundant.
		tag = ""
	case tag == "":
		tag = defaultTag
	}
	if tag != "" {
		if err := validateTag(tag, original); err != nil {
			return Reference{}, err
		}
	}

	return Reference{
		Registry:   registry,
		Repository: repository,
		Tag:        tag,
		Digest:     digest,
		Original:   original,
	}, nil
}

// splitRegistry separates a registry from a repository path, applying the
// dot-colon-or-localhost rule described on ParseReference.
func splitRegistry(name string) (registry, repository string) {
	first, rest, found := strings.Cut(name, "/")
	if !found || !isRegistryHost(first) {
		return defaultRegistry, name
	}
	return first, rest
}

// isRegistryHost reports whether a leading path component names a registry
// rather than the first directory of a repository.
//
// A component carrying a ":" is only a host if what follows is a port. Without
// that check, "alpine:3.20/rc1" — a tag written where a repository path was
// expected — would be read as the host "alpine:3.20" serving the repository
// "rc1", and the user would be told their image does not exist rather than that
// their reference is malformed.
func isRegistryHost(component string) bool {
	host, port, hasPort := strings.Cut(component, ":")
	if hasPort && !isPort(port) {
		return false
	}

	// A host with an empty label is not a host. Without this, "../b" is read as
	// the repository "b" on a registry called ".." and "a..b/c" as "c" on a
	// registry called "a..b" — both of which get as far as a DNS lookup before
	// failing, and report the wrong thing when they do. Falling through leaves
	// them to the repository grammar, which names the actual problem.
	//
	// No real hostname has an empty label, so this rejects nothing that could
	// have worked. It is deliberately the only thing checked here: tightening
	// further would risk refusing an internal registry whose name is unusual
	// but resolvable, and the cost of being wrong in that direction is a pull
	// that cannot be performed at all.
	if host == "" || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") ||
		strings.Contains(host, "..") {
		return false
	}

	return strings.Contains(host, ".") || hasPort || host == "localhost"
}

// isPort reports whether s is a non-empty run of digits.
func isPort(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// validateRepository checks a repository path against the Distribution Spec's
// grammar: lowercase alphanumeric components, separated by "/", each of which
// may contain ".", "_", "__" and runs of "-" between alphanumerics.
//
// The check is deliberately strict rather than permissive. A repository is
// interpolated into a URL path, and an uppercase repository is the single most
// common reason a reference that "looks fine" 404s — reporting it here, by
// name, is far kinder than relaying the registry's error.
func validateRepository(repository, original string) error {
	if repository == "" {
		return fmt.Errorf("%w: %q has no repository", ErrInvalidReference, original)
	}

	for component := range strings.SplitSeq(repository, "/") {
		if err := validateRepositoryComponent(component); err != nil {
			return fmt.Errorf("%w: %q: %w", ErrInvalidReference, original, err)
		}
	}

	return nil
}

// validateRepositoryComponent checks one path component against the
// Distribution Spec's grammar:
//
//	component := alphanumeric [separator alphanumeric]*
//	separator := "." | "_" | "__" | "-"+
//
// The separator rule is the part that is easy to leave out and worth having.
// Implementing it exactly is *less* risky than approximating it with "any of
// . _ - anywhere", because this grammar is what registries themselves enforce:
// a name this rejects is a name no registry would have served, so the strictness
// costs nothing and turns a remote 404 into a local explanation. Fuzzing found
// the approximation accepting "0..0" and "a..b", both of which are invalid.
func validateRepositoryComponent(component string) error {
	if component == "" {
		return errors.New("a repository path component is empty")
	}
	if !isAlphanumeric(component[0]) {
		return fmt.Errorf("repository component %q must start with a lowercase letter or digit", component)
	}

	for i := 0; i < len(component); {
		if isAlphanumeric(component[i]) {
			i++
			continue
		}

		// A separator run. Its length is what the grammar constrains, and a
		// separator must always be followed by something to separate.
		start := i
		switch c := component[i]; c {
		case '.':
			i++
		case '_':
			for i < len(component) && component[i] == '_' {
				i++
			}
			if i-start > 2 {
				return fmt.Errorf("repository component %q has %d underscores in a row; at most two are allowed",
					component, i-start)
			}
		case '-':
			for i < len(component) && component[i] == '-' {
				i++
			}
		default:
			if c >= 'A' && c <= 'Z' {
				return fmt.Errorf("repository names must be lowercase, and %q is not", component)
			}
			return fmt.Errorf("%q is not allowed in a repository name", string(c))
		}

		if i >= len(component) {
			return fmt.Errorf("repository component %q ends with a separator", component)
		}
		if !isAlphanumeric(component[i]) {
			return fmt.Errorf("repository component %q has %q where a letter or digit must follow a separator",
				component, string(component[i]))
		}
	}

	return nil
}

// validateTag checks a tag against the Distribution Spec's grammar: a word
// character followed by up to 127 word characters, dots or dashes.
func validateTag(tag, original string) error {
	if len(tag) > maxTagLength {
		return fmt.Errorf("%w: %q: tag is %d characters, the limit is %d",
			ErrInvalidReference, original, len(tag), maxTagLength)
	}
	if first := tag[0]; !isAlphanumeric(first) && first != '_' && (first < 'A' || first > 'Z') {
		return fmt.Errorf("%w: %q: a tag must begin with a letter, digit or underscore",
			ErrInvalidReference, original)
	}
	for i := 0; i < len(tag); i++ {
		c := tag[i]
		switch {
		case isAlphanumeric(c), c >= 'A' && c <= 'Z', c == '_', c == '.', c == '-':
		default:
			return fmt.Errorf("%w: %q: %q is not allowed in a tag", ErrInvalidReference, original, string(c))
		}
	}
	return nil
}

// isAlphanumeric reports whether c is a lowercase letter or a digit.
func isAlphanumeric(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

// Host returns the hostname serving the registry's HTTP API.
//
// It differs from Registry only for Docker Hub, whose canonical name is
// docker.io and whose API lives at registry-1.docker.io.
func (r Reference) Host() string {
	if r.Registry == defaultRegistry {
		return defaultRegistryHost
	}
	return r.Registry
}

// IsDigest reports whether the reference names immutable content.
//
// It is the difference between a pull that is reproducible and one that is not,
// which is why Resolve treats the two cases differently when verifying.
func (r Reference) IsDigest() bool { return r.Digest != "" }

// Target returns the part of the reference a registry manifest request
// addresses: the digest if there is one, otherwise the tag.
func (r Reference) Target() string {
	if r.IsDigest() {
		return r.Digest
	}
	return r.Tag
}

// String renders the reference in its fully-qualified form, which is what
// makes two references comparable regardless of how they were typed.
func (r Reference) String() string {
	if r.IsDigest() {
		return r.Registry + "/" + r.Repository + "@" + r.Digest
	}
	return r.Registry + "/" + r.Repository + ":" + r.Tag
}
