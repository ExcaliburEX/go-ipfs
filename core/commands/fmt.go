package commands

import (
	"fmt"

	peer "github.com/libp2p/go-libp2p-core/peer"
	mbase "github.com/multiformats/go-multibase"
)

func verifyIDFormatLabel(formatLabel string) error {
	switch formatLabel {
	case "b58mh":
		return nil
	case "b36cid":
		return nil
	}
	return fmt.Errorf("invalid output format option")
}

func formatID(id peer.ID, formatLabel string) string {
	switch formatLabel {
	case "b58mh":
		return id.Pretty()
	case "b36cid":
		if s, err := peer.ToCid(id).StringOfBase(mbase.Base36); err != nil {
			panic(err)
		} else {
			return s
		}
	default:
		panic("unreachable")
	}
}
