package service

import "vornik.io/vornik/internal/version"

// SetEdition records the build edition on the container. Unknown or empty
// values normalize to the community edition (see version.NormalizeEdition).
func (c *Container) SetEdition(e string) { c.edition = version.NormalizeEdition(e) }

// Edition returns the container's build edition, defaulting to the community
// edition when none was stamped.
func (c *Container) Edition() string {
	if c.edition == "" {
		return version.DefaultEdition
	}
	return c.edition
}
