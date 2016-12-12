package server

import (
	"crypto/tls"
	"net/http"
	"os"
	"strconv"
	"time"

	"gopkg.in/airbrake/gobrake.v2"

	"fmt"

	"github.com/Sirupsen/logrus"
	"github.com/jingweno/negroni-gorelic"
	"github.com/julienschmidt/httprouter"
	"github.com/meatballhat/negroni-logrus"
	"github.com/ory-am/hydra/client"
	"github.com/ory-am/hydra/config"
	"github.com/ory-am/hydra/herodot"
	"github.com/ory-am/hydra/jwk"
	"github.com/ory-am/hydra/oauth2"
	"github.com/ory-am/hydra/pkg"
	"github.com/ory-am/hydra/policy"
	"github.com/ory-am/hydra/warden"
	"github.com/ory-am/ladon"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/urfave/negroni"
	"golang.org/x/net/context"
)

var airbrake *gobrake.Notifier

func RunHost(c *config.Config) func(cmd *cobra.Command, args []string) {
	return func(cmd *cobra.Command, args []string) {
		router := httprouter.New()
		serverHandler := &Handler{Config: c}
		serverHandler.registerRoutes(router)
		c.ForceHTTP, _ = cmd.Flags().GetBool("dangerous-force-http")

		if c.ClusterURL == "" {
			proto := "https"
			if c.ForceHTTP {
				proto = "http"
			}
			host := "localhost"
			if c.BindHost != "" {
				host = c.BindHost
			}
			c.ClusterURL = fmt.Sprintf("%s://%s:%d", proto, host, c.BindPort)
		}

		if ok, _ := cmd.Flags().GetBool("dangerous-auto-logon"); ok {
			logrus.Warnln("Do not use flag --dangerous-auto-logon in production.")
			err := c.Persist()
			pkg.Must(err, "Could not write configuration file: %s", err)
		}
		verbose, _ := cmd.Flags().GetBool("new-relic-verbose")

		startCleanUpJob(c)

		n := negroni.New()
		useAirbrakeMiddleware(n)
		useNewRelicMiddleware(n, verbose)
		n.Use(negronilogrus.NewMiddleware())
		n.UseFunc(serverHandler.rejectInsecureRequests)
		n.UseHandler(router)

		var srv = http.Server{
			Addr:    c.GetAddress(),
			Handler: n,
			TLSConfig: &tls.Config{
				Certificates: []tls.Certificate{
					getOrCreateTLSCertificate(cmd, c),
				},
			},
			ReadTimeout:  time.Second * 5,
			WriteTimeout: time.Second * 10,
		}

		var err error
		logrus.Infof("Setting up http server on %s", c.GetAddress())
		if c.ForceHTTP {
			logrus.Warnln("HTTPS disabled. Never do this in production.")
			err = srv.ListenAndServe()
		} else if c.AllowTLSTermination != "" {
			logrus.Infoln("TLS termination enabled, disabling https.")
			err = srv.ListenAndServe()
		} else {
			err = srv.ListenAndServeTLS("", "")
		}
		pkg.Must(err, "Could not start server: %s %s.", err)
	}
}

//Get NewRelic license and app name from environment variables
func useNewRelicMiddleware(n *negroni.Negroni, verbose bool) {
	newRelicLicense := os.Getenv("NEW_RELIC_LICENSE_KEY")
	newRelicApp := os.Getenv("NEW_RELIC_APP_NAME")
	if newRelicLicense != "" && newRelicApp != "" {
		n.Use(negronigorelic.New(newRelicLicense, newRelicApp, verbose))
		logrus.Info("New Relic enabled!")
	} else {
		logrus.Info("New Relic disabled - configs not found")
	}
}

//Get Airbrake ID and key from environment variables
func useAirbrakeMiddleware(n *negroni.Negroni) {
	airbrakeProjectKey := os.Getenv("AIRBRAKE_PROJECT_KEY")
	if os.Getenv("AIRBRAKE_PROJECT_ID") == "" || airbrakeProjectKey == "" {
		logrus.Info("Airbrake disabled - configs not found")
		return
	}
	airbrakeProjectID, err := strconv.ParseInt(os.Getenv("AIRBRAKE_PROJECT_ID"), 10, 64)
	if err != nil {
		logrus.Errorf("Airbrake disabled - error parsing airbrake project ID: %v", err)
		return
	}
	airbrake = gobrake.NewNotifier(airbrakeProjectID, airbrakeProjectKey)
	n.Use(&airbrakeMW{})
	logrus.Info("Airbrake enabled!")
}

func startCleanUpJob(c *config.Config) {
	go func(c *config.Config) {
		logrus.Info("Access token clean up job running in the background")
		for {
			count, _ := c.Context().FositeStore.RemoveOldAccessTokens(c.GetAccessTokenLifespan())
			if count > 0 {
				logrus.Infoln("Deleted " + strconv.FormatInt(count, 10) + " access token(s)")
			}
			time.Sleep(c.GetAccessTokenLifespan())
		}
	}(c)
}

type airbrakeMW struct {
}

func (air *airbrakeMW) ServeHTTP(rw http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	defer func() {
		if err := recover(); err != nil && airbrake != nil {
			defer airbrake.Flush()
			airbrake.Notify(err, r)
		}
	}()
	next(rw, r)
}

type Handler struct {
	Clients *client.Handler
	Keys    *jwk.Handler
	OAuth2  *oauth2.Handler
	Policy  *policy.Handler
	Warden  *warden.WardenHandler
	Config  *config.Config
}

func (h *Handler) registerRoutes(router *httprouter.Router) {
	c := h.Config
	ctx := c.Context()

	// Set up dependencies
	injectJWKManager(c)
	clientsManager := newClientManager(c)
	injectFositeStore(c, clientsManager)
	oauth2Provider := newOAuth2Provider(c, ctx.KeyManager)

	// set up warden
	ctx.Warden = &warden.LocalWarden{
		Warden: &ladon.Ladon{
			Manager: ctx.LadonManager,
		},
		OAuth2:              oauth2Provider,
		Issuer:              c.Issuer,
		AccessTokenLifespan: c.GetAccessTokenLifespan(),
	}

	// Set up handlers
	h.Clients = newClientHandler(c, router, clientsManager)
	h.Keys = newJWKHandler(c, router)
	h.Policy = newPolicyHandler(c, router)
	h.OAuth2 = newOAuth2Handler(c, router, ctx.KeyManager, oauth2Provider)
	h.Warden = warden.NewHandler(c, router)

	router.GET("/health", func(rw http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		rw.WriteHeader(http.StatusNoContent)
	})

	// Create root account if new install
	createRS256KeysIfNotExist(c, oauth2.ConsentEndpointKey, "private", "sig")
	createRS256KeysIfNotExist(c, oauth2.ConsentChallengeKey, "private", "sig")

	h.createRootIfNewInstall(c)
}

func (h *Handler) rejectInsecureRequests(rw http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	if r.TLS != nil || h.Config.ForceHTTP {
		next.ServeHTTP(rw, r)
		return
	}

	if err := h.Config.DoesRequestSatisfyTermination(r); err == nil {
		next.ServeHTTP(rw, r)
		return
	} else {
		logrus.WithError(err).Warnln("Could not serve http connection")
	}

	ans := new(herodot.JSON)
	ans.WriteErrorCode(context.Background(), rw, r, http.StatusBadGateway, errors.New("Can not serve request over insecure http"))
}
