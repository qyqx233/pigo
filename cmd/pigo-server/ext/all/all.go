// Package all imports every Go tool extension in this repository, so that the
// server, by importing this one package, has them all registered. A new
// extension is added here.
package all

import (
	_ "github.com/smallnest/pigo/cmd/pigo-server/ext/allowfetch"
)
