package server // import "github.com/docker/docker/api/server"

import (
	"context"
	"net/http"

	"github.com/containerd/log"
	"github.com/docker/docker/api/server/httpstatus"
	"github.com/docker/docker/api/server/httputils"
	"github.com/docker/docker/api/server/middleware"
	"github.com/docker/docker/api/server/router"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/dockerversion"
	"github.com/docker/docker/internal/otelutil"
	"github.com/gorilla/mux"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/baggage"
)

// versionMatcher defines a variable matcher to be parsed by the router
// when a request is about to be served.
const versionMatcher = "/v{version:[0-9.]+}"

// Server contains instance details for the server
type Server struct {
	middlewares []middleware.Middleware
}

// UseMiddleware appends a new middleware to the request chain.
// This needs to be called before the API routes are configured.
func (s *Server) UseMiddleware(m middleware.Middleware) {
	s.middlewares = append(s.middlewares, m)
}

func (s *Server) makeHTTPHandler(handler httputils.APIFunc, operation string) http.HandlerFunc {
	return otelhttp.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Define the context that we'll pass around to share info
		// like the docker-request-id.
		//
		// The 'context' will be used for global data that should
		// apply to all requests. Data that is specific to the
		// immediate function being called should still be passed
		// as 'args' on the function call.

		// use intermediate variable to prevent "should not use basic type
		// string as key in context.WithValue" golint errors
		ua := r.Header.Get("User-Agent")
		ctx := baggage.ContextWithBaggage(context.WithValue(r.Context(), dockerversion.UAStringKey{}, ua), otelutil.MustNewBaggage(
			otelutil.MustNewMemberRaw(otelutil.TriggerKey, "api"),
		))

		r = r.WithContext(ctx)
		handlerFunc := s.handlerWithGlobalMiddlewares(handler)

		vars := mux.Vars(r)
		if vars == nil {
			vars = make(map[string]string)
		}

		if err := handlerFunc(ctx, w, r, vars); err != nil {
			statusCode := httpstatus.FromError(err)
			if statusCode >= 500 {
				log.G(ctx).Errorf("Handler for %s %s returned error: %v", r.Method, r.URL.Path, err)
			}
			_ = httputils.WriteJSON(w, statusCode, &types.ErrorResponse{
				Message: err.Error(),
			})
		}
	}), operation).ServeHTTP
}

// CreateMux returns a new mux with all the routers registered.
func (s *Server) CreateMux(ctx context.Context, routers ...router.Router) *mux.Router {
	log.G(ctx).Debug("Registering routers")
	m := mux.NewRouter()
	for _, apiRouter := range routers {
		for _, r := range apiRouter.Routes() {
			if ctx.Err() != nil {
				return m
			}
			log.G(ctx).WithFields(log.Fields{"method": r.Method(), "path": r.Path()}).Debug("Registering route")
			f := s.makeHTTPHandler(r.Handler(), r.Method()+" "+r.Path())
			m.Path(versionMatcher + r.Path()).Methods(r.Method()).Handler(f)
			m.Path(r.Path()).Methods(r.Method()).Handler(f)
		}
	}

	// Setup handlers for undefined paths and methods
	notFoundHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = httputils.WriteJSON(w, http.StatusNotFound, &types.ErrorResponse{
			Message: "page not found",
		})
	})

	m.HandleFunc(versionMatcher+"/{path:.*}", notFoundHandler)
	m.NotFoundHandler = notFoundHandler
	m.MethodNotAllowedHandler = notFoundHandler

	return m
}
