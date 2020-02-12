package jwt

import (
	"fmt"
	"log"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/caddyauth"
	"go.uber.org/zap"
)

// RuleType distinguishes between ALLOW and DENY rules
type RuleType int

const (
	// ALLOW represents a rule that should allow access based on claim value
	ALLOW RuleType = iota

	// DENY represents a rule that should deny access based on claim value
	DENY
)

// EncryptionType specifies the valid configuration for a path
type EncryptionType int

const (
	// HS family of algorithms
	HMAC EncryptionType = iota + 1
	// RS and ES families of algorithms
	PKI
)

// Auth represents configuration information for the middleware
type Auth struct {
	Realm        string             `json:"realm,omitempty"`
	AccessRules  []AccessRule       `json:"access_rules,omitempty"`
	Redirect     string             `json:"redirect,omitempty"`
	KeyBackends  []KeyBackendHolder `json:"key_backends,omitempty"`
	Passthrough  bool               `json:"passthrough"`
	StripHeader  bool               `json:"strip_header"`
	TokenSources []TokenSource      `json:"token_sources,omitempty"`
	Logger       *zap.Logger        `json:"-"`
}

var (
	_ caddy.Module            = (*Auth)(nil)
	_ caddy.Provisioner       = (*Auth)(nil)
	_ caddy.Validator         = (*Auth)(nil)
	_ caddyauth.Authenticator = (*Auth)(nil)
)

// AccessRule represents a single ALLOW/DENY rule based on the value of a claim in
// a validated token
type AccessRule struct {
	Authorize RuleType `json:"authorize"`
	Claim     string   `json:"claim"`
	Value     string   `json:"value"`
}

// CaddyModule returns the Caddy module information.
func (Auth) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.authentication.providers.jwt",
		New: func() caddy.Module { return new(Auth) },
	}
}

func (auth *Auth) Provision(ctx caddy.Context) error {
	auth.Logger = ctx.Logger(auth)
	return nil
}

func (auth *Auth) Validate() error {
	// check all rules at least have a consistent encryption config
	var encType EncryptionType
	for _, e := range auth.KeyBackends {
		switch e.Value.(type) {
		case *HmacKeyBackend, *LazyHmacKeyBackend, *EnvHmacKeyBackend:
			if encType > 0 && encType != HMAC {
				return fmt.Errorf("Configuration does not have a consistent encryption type.  Cannot use both HMAC and PKI for a single path value.")
			}
			encType = HMAC
		case *PublicKeyBackend, *LazyPublicKeyBackend:
			if encType > 0 && encType != PKI {
				return fmt.Errorf("Configuration does not have a consistent encryption type.  Cannot use both HMAC and PKI for a single path value.")
			}
			encType = PKI
		}
	}

	return nil
}

func init() {
	err := caddy.RegisterModule(Auth{})
	if err != nil {
		log.Fatal(err)
	}
	httpcaddyfile.RegisterHandlerDirective("jwt", parseCaddyfile)
}

// parseCaddyfile sets up the handler from Caddyfile tokens. Syntax:
//
//     jwt [<matcher>] {
//         allow <claim> <value>
//         deny <claim> <value>
//         redirect <path>
//         publickey <path>
//         secret <path>
//         passthrough
//         strip_header
//         token_source header <header_name>
//         token_source cookie <cookie_name>
//         token_source query_param <param_name>
//     }
//
// If no hash algorithm is supplied, bcrypt will be assumed.
func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	defaultKeyBackends, err := NewDefaultKeyBackends()
	if err != nil {
		return nil, err
	}

	var r = Auth{
		KeyBackends: defaultKeyBackends,
	}

	// JWT token.
	if !h.Next() {
		return nil, h.ArgErr()
	}

	// Matcher gets removed, so we don't need to care about it.

	args := h.RemainingArgs()
	switch len(args) {
	case 0:
		// no argument passed, check the config block
		for nesting := h.Nesting(); h.NextBlock(nesting); {
			switch h.Val() {
			case "allow":
				args1 := h.RemainingArgs()
				if len(args1) != 2 {
					return nil, h.ArgErr()
				}
				r.AccessRules = append(r.AccessRules, AccessRule{Authorize: ALLOW, Claim: args1[0], Value: args1[1]})
			case "deny":
				args1 := h.RemainingArgs()
				if len(args1) != 2 {
					return nil, h.ArgErr()
				}
				r.AccessRules = append(r.AccessRules, AccessRule{Authorize: DENY, Claim: args1[0], Value: args1[1]})
			case "redirect":
				args1 := h.RemainingArgs()
				if len(args1) != 1 {
					return nil, h.ArgErr()
				}
				r.Redirect = args1[0]
			case "publickey":
				args1 := h.RemainingArgs()
				if len(args1) != 1 {
					return nil, h.ArgErr()
				}
				backend, err := NewLazyPublicKeyFileBackend(args1[0])
				if err != nil {
					return nil, h.Err(err.Error())
				}
				r.KeyBackends = append(r.KeyBackends, *backend)
			case "secret":
				args1 := h.RemainingArgs()
				if len(args1) != 1 {
					return nil, h.ArgErr()
				}
				backend, err := NewLazyHmacKeyBackend(args1[0])
				if err != nil {
					return nil, h.Err(err.Error())
				}
				r.KeyBackends = append(r.KeyBackends, *backend)
			case "passthrough":
				r.Passthrough = true
			case "strip_header":
				r.StripHeader = true
			case "token_source":
				args := h.RemainingArgs()
				if len(args) < 1 {
					return nil, h.ArgErr()
				}
				switch args[0] {
				case "header":
					var headerSource = &HeaderTokenSource{
						HeaderName: "Bearer",
					}
					if len(args) == 2 {
						headerSource.HeaderName = args[1]
					} else if len(args) > 2 {
						return nil, h.ArgErr()
					}
					r.TokenSources = append(r.TokenSources, headerSource)
				case "cookie":
					if len(args) != 2 {
						return nil, h.ArgErr()
					}
					r.TokenSources = append(r.TokenSources, &CookieTokenSource{
						CookieName: args[1],
					})
				case "query_param":
					if len(args) != 2 {
						return nil, h.ArgErr()
					}
					r.TokenSources = append(r.TokenSources, &QueryTokenSource{
						ParamName: args[1],
					})
				default:
					return nil, h.Errf("unsupported token_source: '%s'", args[0])
				}
			}
		}
	default:
		// we want only block arguments
		//return nil, h.ArgErr()
		return nil, h.Errf("unexpected args: '%v'", args)
	}

	return caddyauth.Authentication{
		ProvidersRaw: caddy.ModuleMap{
			"jwt": caddyconfig.JSON(r, nil),
		},
	}, nil
}