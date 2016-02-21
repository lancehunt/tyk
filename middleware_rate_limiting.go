package main

import "net/http"

import (
	"errors"
	"github.com/Sirupsen/logrus"
	"github.com/gorilla/context"
)

var sessionLimiter = SessionLimiter{}
var sessionMonitor = Monitor{}

// RateLimitAndQuotaCheck will check the incomming request and key whether it is within it's quota and
// within it's rate limit, it makes use of the SessionLimiter object to do this
type RateLimitAndQuotaCheck struct {
	*TykMiddleware
}

// New lets you do any initialisations for the object can be done here
func (k *RateLimitAndQuotaCheck) New() {}

// GetConfig retrieves the configuration from the API config - we user mapstructure for this for simplicity
func (k *RateLimitAndQuotaCheck) GetConfig() (interface{}, error) {
	return nil, nil
}

// ProcessRequest will run any checks on the request on the way through the system, return an error to have the chain fail
func (k *RateLimitAndQuotaCheck) ProcessRequest(w http.ResponseWriter, r *http.Request, configuration interface{}) (error, int) {
	// 1. Get base session for this Request
	thisSessionState := context.Get(r, SessionData).(SessionState)
	authHeaderValue := context.Get(r, AuthHeaderValue).(string)
	// 2. If base session has policy_per_api map for current API
	if thisSessionState.PolicyPerAPI != nil {
		apiSessionKey := authHeaderValue+APISessionKeySuffix+k.Spec.APIID;
		perAPISession, found := k.Spec.SessionManager.GetSessionDetail(apiSessionKey)
		if found {
			//    a. Apply Limiter logic to per-api session only
			return applyRateLimiting(apiSessionKey, perAPISession, k, r, configuration)
			// REVIEW: should this return the baseSession or the api session????
		}
		// REVIEW: should this fall-through when not-found?
	}

	//   Else...
	//    b. Apply limiter logic to base session
	return applyRateLimiting(authHeaderValue, thisSessionState, k, r, configuration)
}

func applyRateLimiting(key string, thisSessionState SessionState, k *RateLimitAndQuotaCheck, r *http.Request, configuration interface{}) (error, int) {
	storeRef := k.Spec.SessionManager.GetStore()
	forwardMessage, reason := sessionLimiter.ForwardMessage(&thisSessionState, key, storeRef)

	// Ensure quota and rate data for this session are recorded
	if !config.UseAsyncSessionWrite {
		k.Spec.SessionManager.UpdateSession(key, thisSessionState, 0)
		context.Set(r, SessionData, thisSessionState)
	} else {
		go k.Spec.SessionManager.UpdateSession(key, thisSessionState, 0)
		go context.Set(r, SessionData, thisSessionState)
	}

	log.Debug("SessionState: ", thisSessionState)

	if !forwardMessage {
		// TODO Use an Enum!
		if reason == 1 {
			log.WithFields(logrus.Fields{
				"path":   r.URL.Path,
				"origin": r.RemoteAddr,
				"key":    key,
			}).Info("Key rate limit exceeded.")

			// Fire a rate limit exceeded event
			go k.TykMiddleware.FireEvent(EVENT_RateLimitExceeded,
				EVENT_RateLimitExceededMeta{
					EventMetaDefault: EventMetaDefault{Message: "Key Rate Limit Exceeded", OriginatingRequest: EncodeRequestToEvent(r)},
					Path:             r.URL.Path,
					Origin:           r.RemoteAddr,
					Key:              key,
				})

			// Report in health check
			ReportHealthCheckValue(k.Spec.Health, Throttle, "-1")

			return errors.New("Rate limit exceeded"), 429

		} else if reason == 2 {
			log.WithFields(logrus.Fields{
				"path":   r.URL.Path,
				"origin": r.RemoteAddr,
				"key":    key,
			}).Info("Key quota limit exceeded.")

			// Fire a quota exceeded event
			go k.TykMiddleware.FireEvent(EVENT_QuotaExceeded,
				EVENT_QuotaExceededMeta{
					EventMetaDefault: EventMetaDefault{Message: "Key Quota Limit Exceeded", OriginatingRequest: EncodeRequestToEvent(r)},
					Path:             r.URL.Path,
					Origin:           r.RemoteAddr,
					Key:              key,
				})

			// Report in health check
			ReportHealthCheckValue(k.Spec.Health, QuotaViolation, "-1")

			return errors.New("Quota exceeded"), 403
		}
		// Other reason? Still not allowed
		return errors.New("Access denied"), 403
	}

	// Run the trigger monitor
	if config.Monitor.MonitorUserKeys {
		sessionMonitor.Check(&thisSessionState, key)
	}

	// Request is valid, carry on
	return nil, 200
}
