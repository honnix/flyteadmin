package auth

import (
	"context"
	"github.com/coreos/go-oidc"
	"github.com/lyft/flytestdlib/errors"
	"github.com/lyft/flytestdlib/logger"
	"golang.org/x/oauth2"
	"io/ioutil"
	"net/http"
	"strings"
)

type AuthenticationContext interface {
	OAuth2Config() *oauth2.Config
	Claims() Claims
	RedirectUrl() string
	OidcProvider() *oidc.Provider
	CookieManager() CookieManager
	HttpAuthorizationHeader() string
	GrpcAuthorizationHeader() string
}

type Context struct {
	oauth2                  *oauth2.Config
	redirectUrl             string
	claims                  Claims
	cookieManager           CookieManager
	oidcProvider            *oidc.Provider
	httpAuthorizationHeader string
	grpcAuthorizationHeader string
}

func (c Context) OAuth2Config() *oauth2.Config {
	return c.oauth2
}

func (c Context) Claims() Claims {
	return c.claims
}

func (c Context) RedirectUrl() string {
	return c.redirectUrl
}

func (c Context) OidcProvider() *oidc.Provider {
	return c.oidcProvider
}

func (c Context) CookieManager() CookieManager {
	return c.cookieManager
}

func (c Context) HttpAuthorizationHeader() string {
	return c.httpAuthorizationHeader
}

func (c Context) GrpcAuthorizationHeader() string {
	return c.grpcAuthorizationHeader
}

const (
	ErrAuthContext errors.ErrorCode = "AUTH_CONTEXT_SETUP_FAILED"
)

func NewAuthenticationContext(ctx context.Context, options OAuthOptions) (Context, error) {
	oauth2Config, err := GetOauth2Config(options)
	if err != nil {
		return Context{}, errors.Wrapf(ErrAuthContext, err, "Error creating OAuth2 library configuration")
	}

	cookieManager, err := NewCookieManager(ctx, options.CookieHashKeyFile, options.CookieBlockKeyFile)
	if err != nil {
		logger.Errorf(ctx, "Error creating cookie manager %s", err)
		return Context{}, errors.Wrapf(ErrAuthContext, err, "Error creating cookie manager")
	}

	oidcCtx := oidc.ClientContext(ctx, &http.Client{})
	provider, err := oidc.NewProvider(oidcCtx, options.Claims.Issuer)
	if err != nil {
		return Context{}, errors.Wrapf(ErrAuthContext, err, "Error creating oidc provider")
	}

	var httpAuthorizationHeader = DefaultAuthorizationHeader
	var grpcAuthorizationHeader = DefaultAuthorizationHeader
	if options.HttpAuthorizationHeader != "" {
		httpAuthorizationHeader = options.HttpAuthorizationHeader
	}
	if options.GrpcAuthorizationHeader != "" {
		grpcAuthorizationHeader = options.GrpcAuthorizationHeader
	}

	return Context{
		oauth2:                  &oauth2Config,
		redirectUrl:             options.RedirectUrl,
		claims:                  options.Claims,
		cookieManager:           cookieManager,
		oidcProvider:            provider,
		httpAuthorizationHeader: httpAuthorizationHeader,
		grpcAuthorizationHeader: grpcAuthorizationHeader,
	}, nil
}

// This creates a oauth2 library config object, with values from the Flyte Admin config
func GetOauth2Config(options OAuthOptions) (oauth2.Config, error) {
	secretBytes, err := ioutil.ReadFile(options.ClientSecretFile)
	if err != nil {
		return oauth2.Config{}, err
	}
	secret := strings.TrimSuffix(string(secretBytes), "\n")
	return oauth2.Config{
		RedirectURL:  options.CallbackUrl,
		ClientID:     options.ClientId,
		ClientSecret: secret,
		// Offline access needs to be specified in order to return a refresh token in the exchange.
		// TODO: Second parameter is IDP specific - move to config. Also handle case where a refresh token is not allowed
		Scopes: []string{OidcScope, OfflineAccessType},
		Endpoint: oauth2.Endpoint{
			AuthURL:  options.AuthorizeUrl,
			TokenURL: options.TokenUrl,
		},
	}, nil
}