// bridges.go — adapters to the three embedded bridge packages. The router
// core never reaches into their internals: Init() prepares each bridge's
// state, Handler() hands back its full HTTP surface.

package core

import (
	"net/http"

	"github.com/Godde3s/omnirouter/internal/dsbridge"
	"github.com/Godde3s/omnirouter/internal/gbridge"
	"github.com/Godde3s/omnirouter/internal/obridge"
	"github.com/Godde3s/omnirouter/internal/qbridge"
	"github.com/Godde3s/omnirouter/internal/zbridge"
)

func zbInit()                 { zbridge.Init() }
func zbHandler() http.Handler { return zbridge.Handler() }

func qbInit()                 { qbridge.Init() }
func qbHandler() http.Handler { return qbridge.Handler() }

func dsInit()                 { dsbridge.Init() }
func dsHandler() http.Handler { return dsbridge.Handler() }

func gbInit()                 { gbridge.Init() }
func gbHandler() http.Handler { return gbridge.Handler() }

func obInit()                 { obridge.Init() }
func obHandler() http.Handler { return obridge.Handler() }
