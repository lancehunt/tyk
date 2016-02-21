package main

import (
	"bytes"
	b64 "encoding/base64"
	"io"
	"net/http"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/context"
	"github.com/pmylund/go-cache"
)

// ContextKey is a key type to avoid collisions
type ContextKey int

// Enums for keys to be stored in a session context - this is how gorilla expects
// these to be implemented and is lifted pretty much from docs
const (
	SessionData       = 0
	AuthHeaderValue   = 1
	VersionData       = 2
	VersionKeyContext = 3
)

const APISessionKeySuffix = ".API-"

var SessionCache *cache.Cache = cache.New(10*time.Second, 5*time.Second)

type ReturningHttpHandler interface {
	ServeHTTP(http.ResponseWriter, *http.Request) *http.Response
	ServeHTTPForCache(http.ResponseWriter, *http.Request) *http.Response
	CopyResponse(io.Writer, io.Reader)
	New(interface{}, *APISpec) (TykResponseHandler, error)
}

// TykMiddleware wraps up the ApiSpec and Proxy objects to be included in a
// middleware handler, this can probably be handled better.
type TykMiddleware struct {
	Spec  *APISpec
	Proxy ReturningHttpHandler
}

func SetUpSessionCache() *cache.Cache {
	sessionLength := 10
	evictionTime := 5
	if config.LocalSessionCache.CachedSessionTimeout > 0 {
		sessionLength = config.LocalSessionCache.CachedSessionTimeout
	}
	if config.LocalSessionCache.CacheSessionEviction > 0 {
		evictionTime = config.LocalSessionCache.CacheSessionEviction
	}

	return cache.New(time.Duration(sessionLength)*time.Second, time.Duration(evictionTime)*time.Second)
}

func (t TykMiddleware) GetOrgSession(key string) (SessionState, bool) {
	// Try and get the session from the session store
	var thisSession SessionState
	var found bool

	thisSession, found = t.Spec.OrgSessionManager.GetSessionDetail(key)
	if found {
		// If exists, assume it has been authorized and pass on
		return thisSession, true
	}

	return thisSession, found
}

// ApplyPolicyIfExists will check if a policy is loaded, if it is, it will overwrite the session state to use the policy values
func (t TykMiddleware) ApplyPolicyIfExists(key string, thisSession *SessionState, stripPolicyID bool) {
	if thisSession.ApplyPolicyID != "" {
		log.Debug("Session has policy, checking")
		policy, ok := Policies[thisSession.ApplyPolicyID]
		if ok {
			// Check ownership, policy org owner must be the same as API,
			// otherwise youcould overwrite a session key with a policy from a different org!
			if policy.OrgID != t.Spec.APIDefinition.OrgID {
				log.Error("Attempting to apply policy from different organisation to key, skipping")
				return
			}

			log.Debug("Found policy, applying")
			thisSession.Allowance = policy.Rate // This is a legacy thing, merely to make sure output is consistent. Needs to be purged
			thisSession.Rate = policy.Rate
			thisSession.Per = policy.Per
			thisSession.QuotaMax = policy.QuotaMax
			thisSession.QuotaRenewalRate = policy.QuotaRenewalRate
			thisSession.PolicyPerAPI = policy.PolicyPerAPI
			thisSession.AccessRights = policy.AccessRights
			thisSession.HMACEnabled = policy.HMACEnabled
			thisSession.IsInactive = policy.IsInactive
			thisSession.Tags = policy.Tags

			// Usually the reason to remove is because it was a temporary policy ID
			if stripPolicyID {
				thisSession.ApplyPolicyID = ""
			}

			// Update the session in the session manager in case it gets called again
			t.Spec.SessionManager.UpdateSession(key, *thisSession, t.Spec.APIDefinition.SessionLifetime)
			log.Debug("Policy applied to key")
		}
	}
}

// CheckSessionAndIdentityForValidKey will check first the Session store for a valid key, if not found, it will try
// the Auth Handler, if not found it will fail
func (t TykMiddleware) CheckSessionAndIdentityForValidKey(key string) (SessionState, bool) {
	// 1. Try and get the base session
	baseSession, baseFound := checkSessionAndValidateKey(key, t)
	// 2. If base session has policy_per_api map for current API being requested
	if baseFound {
		if baseSession.PolicyPerAPI != nil {
			// 2a. Check current per-api session
			apiPolicyID := baseSession.PolicyPerAPI[t.Spec.APIID]
			if apiPolicyID != "" {
				// Ensure per-api session is setup to match base session
				apiSessionKey := key + APISessionKeySuffix + t.Spec.APIID
				perApiSession, apiSessionFound := checkSessionAndValidateKey(apiSessionKey, t)
				// If not, create a per-api session to allow tracking policy at the API level
				if !apiSessionFound {
					// find policy and build new SessionState based upon it
					perApiSession.ApplyPolicyID = apiPolicyID
					t.ApplyPolicyIfExists(apiSessionKey, &perApiSession, true)
				}
			}
		}
	}

	// 3. Return base session
	return baseSession, baseFound
}

func checkSessionAndValidateKey(key string, t TykMiddleware) (SessionState, bool) {
	var thisSession SessionState
	var found bool

	// 1. Check in-memory cache
	if !config.LocalSessionCache.DisableCacheSessionState {
		cachedVal, found := SessionCache.Get(key)
		if found {
			log.Debug("Key found in local cache")
			thisSession = cachedVal.(SessionState)
			t.ApplyPolicyIfExists(key, &thisSession, false)
			return thisSession, true
		}
	}

	// 2. Check session store
	thisSession, found = t.Spec.SessionManager.GetSessionDetail(key)
	if found {
		// If exists, assume it has been authorized and pass on
		// cache it
		go SessionCache.Set(key, thisSession, cache.DefaultExpiration)

		// Check for a policy, if there is a policy, pull it and overwrite the session values
		t.ApplyPolicyIfExists(key, &thisSession, false)
		return thisSession, true
	}

	// 3. If not there, get it from the AuthorizationHandler
	thisSession, found = t.Spec.AuthManager.IsKeyAuthorised(key)
	if found {
		// If not in Session, and got it from AuthHandler, create a session with a new TTL
		log.Info("Recreating session for key: ", key)

		// cache it
		go SessionCache.Set(key, thisSession, cache.DefaultExpiration)

		// Check for a policy, if there is a policy, pull it and overwrite the session values
		t.ApplyPolicyIfExists(key, &thisSession, false)
		t.Spec.SessionManager.UpdateSession(key, thisSession, t.Spec.APIDefinition.SessionLifetime)
	}
	return thisSession, found
}

// SuccessHandler represents the final ServeHTTP() request for a proxied API request
type SuccessHandler struct {
	*TykMiddleware
}

func (s SuccessHandler) RecordHit(w http.ResponseWriter, r *http.Request, timing int64, code int, requestCopy *http.Request, responseCopy *http.Response) {

	if s.Spec.DoNotTrack {
		return
	}

	if config.StoreAnalytics(r) {

		t := time.Now()

		// Track the key ID if it exists
		authHeaderValue := context.Get(r, AuthHeaderValue)
		keyName := ""
		if authHeaderValue != nil {
			keyName = authHeaderValue.(string)
		}

		// Track version data
		version := s.Spec.getVersionFromRequest(r)
		if version == "" {
			version = "Non Versioned"
		}

		// If OAuth, we need to grab it from the session, which may or may not exist
		OauthClientID := ""
		tags := make([]string, 0)
		thisSessionState := context.Get(r, SessionData)

		if thisSessionState != nil {
			OauthClientID = thisSessionState.(SessionState).OauthClientID
			tags = thisSessionState.(SessionState).Tags
		}

		rawRequest := ""
		rawResponse := ""
		if config.AnalyticsConfig.EnableDetailedRecording {
			if requestCopy != nil {
				// Get the wire format representation
				var wireFormatReq bytes.Buffer
				requestCopy.Write(&wireFormatReq)
				rawRequest = b64.StdEncoding.EncodeToString(wireFormatReq.Bytes())
			}
			if responseCopy != nil {
				// Get the wire format representation
				var wireFormatRes bytes.Buffer
				responseCopy.Write(&wireFormatRes)
				rawResponse = b64.StdEncoding.EncodeToString(wireFormatRes.Bytes())
			}
		}

		thisRecord := AnalyticsRecord{
			r.Method,
			r.URL.Path,
			r.ContentLength,
			r.Header.Get("User-Agent"),
			t.Day(),
			t.Month(),
			t.Year(),
			t.Hour(),
			code,
			keyName,
			t,
			version,
			s.Spec.APIDefinition.Name,
			s.Spec.APIDefinition.APIID,
			s.Spec.APIDefinition.OrgID,
			OauthClientID,
			timing,
			rawRequest,
			rawResponse,
			tags,
			time.Now(),
		}

		expiresAfter := s.Spec.ExpireAnalyticsAfter
		if config.EnforceOrgDataAge {
			thisOrg := s.Spec.OrgID
			orgSessionState, found := s.GetOrgSession(thisOrg)
			if found {
				if orgSessionState.DataExpires > 0 {
					expiresAfter = orgSessionState.DataExpires
				}
			}
		}

		thisRecord.SetExpiry(expiresAfter)

		go analytics.RecordHit(thisRecord)
	}

	// Report in health check
	ReportHealthCheckValue(s.Spec.Health, RequestLog, strconv.FormatInt(int64(timing), 10))

	if doMemoryProfile {
		pprof.WriteHeapProfile(profileFile)
	}

	context.Clear(r)
}

// ServeHTTP will store the request details in the analytics store if necessary and proxy the request to it's
// final destination, this is invoked by the ProxyHandler or right at the start of a request chain if the URL
// Spec states the path is Ignored
func (s SuccessHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) *http.Response {
	// Make sure we get the correct target URL
	if s.Spec.APIDefinition.Proxy.StripListenPath {
		log.Debug("Stripping: ", s.Spec.Proxy.ListenPath)
		r.URL.Path = strings.Replace(r.URL.Path, s.Spec.Proxy.ListenPath, "", 1)
		log.Debug("Upstream Path is: ", r.URL.Path)
	}

	var copiedRequest *http.Request
	if config.AnalyticsConfig.EnableDetailedRecording {
		copiedRequest = CopyHttpRequest(r)
	}

	t1 := time.Now()
	resp := s.Proxy.ServeHTTP(w, r)
	t2 := time.Now()

	var copiedResponse *http.Response
	if config.AnalyticsConfig.EnableDetailedRecording {
		copiedResponse = CopyHttpResponse(resp)
	}

	millisec := float64(t2.UnixNano()-t1.UnixNano()) * 0.000001
	log.Debug("Upstream request took (ms): ", millisec)

	if resp != nil {
		s.RecordHit(w, r, int64(millisec), resp.StatusCode, copiedRequest, copiedResponse)
	}

	return nil
}

// ServeHTTPWithCache will store the request details in the analytics store if necessary and proxy the request to it's
// final destination, this is invoked by the ProxyHandler or right at the start of a request chain if the URL
// Spec states the path is Ignored Itwill also return a response object for the cache
func (s SuccessHandler) ServeHTTPWithCache(w http.ResponseWriter, r *http.Request) *http.Response {
	// Make sure we get the correct target URL
	if s.Spec.APIDefinition.Proxy.StripListenPath {
		r.URL.Path = strings.Replace(r.URL.Path, s.Spec.Proxy.ListenPath, "", 1)
	}

	var copiedRequest *http.Request
	if config.AnalyticsConfig.EnableDetailedRecording {
		copiedRequest = CopyHttpRequest(r)
	}

	t1 := time.Now()
	inRes := s.Proxy.ServeHTTPForCache(w, r)
	t2 := time.Now()

	var copiedResponse *http.Response
	if config.AnalyticsConfig.EnableDetailedRecording {
		copiedResponse = CopyHttpResponse(inRes)
	}

	millisec := float64(t2.UnixNano()-t1.UnixNano()) * 0.000001
	log.Debug("Upstream request took (ms): ", millisec)

	if inRes != nil {
		s.RecordHit(w, r, int64(millisec), inRes.StatusCode, copiedRequest, copiedResponse)
	}

	return inRes
}
