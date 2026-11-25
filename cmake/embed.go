// Package cmake embeds kiln.cmake so the kiln tools can install it.
package cmake

import _ "embed"

// Module is the contents of kiln.cmake.
//
//go:embed kiln.cmake
var Module []byte
