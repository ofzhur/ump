// Package all registers every provider type that ships in the binary.
// Import it for its side effect:
//
//	import _ "github.com/ofzhur/ump/internal/provider/all"
package all

import (
	"github.com/ofzhur/ump/internal/provider"
	"github.com/ofzhur/ump/internal/provider/hetznercloud"
	"github.com/ofzhur/ump/internal/provider/hetznerrobot"
	"github.com/ofzhur/ump/internal/provider/linode"
	"github.com/ofzhur/ump/internal/provider/manual"
	"github.com/ofzhur/ump/internal/provider/ovh"
	"github.com/ofzhur/ump/internal/provider/vultr"
)

func init() {
	provider.Register(hetznerrobot.Type)
	provider.Register(hetznercloud.Type)
	provider.Register(ovh.Type)
	provider.Register(linode.Type)
	provider.Register(vultr.Type)
	provider.Register(manual.Type)
}
