// Package all registers every provider type that ships in the binary.
// Import it for its side effect:
//
//	import _ "github.com/ofzhur/ump/internal/provider/all"
package all

import (
	"github.com/ofzhur/ump/internal/provider"
	"github.com/ofzhur/ump/internal/provider/hetznercloud"
	"github.com/ofzhur/ump/internal/provider/hetznerrobot"
	"github.com/ofzhur/ump/internal/provider/ovh"
)

func init() {
	provider.Register(hetznerrobot.Type)
	provider.Register(hetznercloud.Type)
	provider.Register(ovh.Type)
}
