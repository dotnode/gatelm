package console

import (
	"net/http"

	"github.com/dotnode/gatelm/pkg/gatelm"
)

type Options struct {
	BasePath string
}

func Mount(mux *http.ServeMux, gateway *gatelm.Gateway, opts Options) {
	gateway.RegisterConsoleRoutes(mux, opts.BasePath)
}
