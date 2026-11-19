package oidc

import "strconv"

// An identity provider controls much of what this package reads: answer
// bodies, error codes and descriptions, the status line, discovery
// documents, key sets, and every member of a token. Any of it can hold a
// credential, because a provider or a proxy can repeat what the request
// carried. So no failure of this package carries text: it carries a
// Refusal, whose fields are enums and numbers Writ computed, and whose
// message Writ writes. A value from a remote party is never formatted into
// it. When the value matters to an operator, the message says what kind it
// was, never what it was.

// Refusal is why an issuer, a sign-in, or a token was refused. Every field
// is an enum or a number Writ computed, so it holds nothing an identity
// provider or a token sent. It is not an error: it cannot be wrapped, and
// its only text is Message.
type Refusal struct {
	stage  Stage
	check  Check
	reason Reason
	field  Field
	code   OAuthCode
	// status is the HTTP status of the answer, when the reason names one.
	status int
	// key is the 1-based position of a key in a key set.
	key int
	// count and limit are a length, a count, or a size, and its bound.
	count, limit int64
}

// Stage is where a refusal happened: Writ's own config, an endpoint of the
// identity provider, or a token's checks.
type Stage uint8

const (
	StageConfig Stage = iota + 1
	StageDiscovery
	StageKeySet
	StageAuthorization
	StageDeviceAuthorization
	StageTokenEndpoint
	StageUserinfo
	StageToken
)

var stageNames = [...]string{
	StageConfig:              "config",
	StageDiscovery:           "discovery",
	StageKeySet:              "key set",
	StageAuthorization:       "authorization endpoint",
	StageDeviceAuthorization: "device authorization",
	StageTokenEndpoint:       "token endpoint",
	StageUserinfo:            "userinfo",
	StageToken:               "token",
}

func (s Stage) String() string { return lookup(stageNames[:], int(s)) }

// Check names the check a token failed, in the order Verify runs them.
type Check uint8

const (
	CheckShape Check = iota + 1
	CheckHeader
	CheckIssuer
	CheckSignature
	CheckTimes
	CheckAudience
	CheckClaims
	// CheckNonce is a sign-in whose ID token carries another nonce than
	// the one the browser's sign-in started with.
	CheckNonce
)

var checkNames = [...]string{
	CheckShape: "shape", CheckHeader: "header", CheckIssuer: "issuer", CheckSignature: "signature",
	CheckTimes: "times", CheckAudience: "audience", CheckClaims: "claims", CheckNonce: "nonce",
}

func (c Check) String() string { return lookup(checkNames[:], int(c)) }

// Field is a member of a token, a discovery document, a key set, or an
// answer, named in a refusal. Writ names the member; the value never shows.
type Field uint8

const (
	FieldHeader Field = iota + 1
	FieldPayload
	FieldSignature
	FieldAlg
	FieldKid
	FieldJku
	FieldJwk
	FieldX5u
	FieldX5c
	FieldCrit
	FieldB64
	FieldEnc
	FieldZip
	FieldEpk
	FieldIss
	FieldAud
	FieldAzp
	FieldSub
	FieldJti
	FieldExp
	FieldIat
	FieldNbf
	FieldNonce
	FieldProjectID
	FieldProjectPath
	FieldNamespaceID
	FieldPipelineID
	FieldPipelineSource
	FieldJobID
	FieldRefPath
	FieldRefProtected
	FieldSha
	FieldUserID
	FieldUserLogin
	FieldRepository
	FieldRepositoryID
	FieldRepositoryOwnerID
	FieldRunID
	FieldRef
	FieldEventName
	FieldWorkflowRef
	FieldActor
	FieldActorID
	FieldJWKSURI
	FieldAuthorizationEndpoint
	FieldTokenEndpoint
	FieldUserinfoEndpoint
	FieldDeviceEndpoint
	FieldDeviceCode
	FieldUserCode
	FieldExpiresIn
	FieldVerificationURI
	FieldIDToken
)

var fieldNames = [...]string{
	FieldHeader: "header", FieldPayload: "payload", FieldSignature: "signature",
	FieldAlg: "alg", FieldKid: "kid", FieldJku: "jku", FieldJwk: "jwk", FieldX5u: "x5u", FieldX5c: "x5c",
	FieldCrit: "crit", FieldB64: "b64", FieldEnc: "enc", FieldZip: "zip", FieldEpk: "epk",
	FieldIss: "iss", FieldAud: "aud", FieldAzp: "azp", FieldSub: "sub", FieldJti: "jti",
	FieldExp: "exp", FieldIat: "iat", FieldNbf: "nbf", FieldNonce: "nonce",
	FieldProjectID: "project_id", FieldProjectPath: "project_path", FieldNamespaceID: "namespace_id",
	FieldPipelineID: "pipeline_id", FieldPipelineSource: "pipeline_source", FieldJobID: "job_id",
	FieldRefPath: "ref_path", FieldRefProtected: "ref_protected", FieldSha: "sha", FieldUserID: "user_id",
	FieldUserLogin: "user_login", FieldRepository: "repository", FieldRepositoryID: "repository_id",
	FieldRepositoryOwnerID: "repository_owner_id", FieldRunID: "run_id", FieldRef: "ref",
	FieldEventName: "event_name", FieldWorkflowRef: "workflow_ref", FieldActor: "actor", FieldActorID: "actor_id",
	FieldJWKSURI: "jwks_uri", FieldAuthorizationEndpoint: "authorization_endpoint", FieldTokenEndpoint: "token_endpoint",
	FieldUserinfoEndpoint: "userinfo_endpoint", FieldDeviceEndpoint: "device_authorization_endpoint",
	FieldDeviceCode: "device_code", FieldUserCode: "user_code", FieldExpiresIn: "expires_in",
	FieldVerificationURI: "verification_uri", FieldIDToken: "id_token",
}

// Name is the field's name as the protocol spells it.
func (f Field) Name() string { return lookup(fieldNames[:], int(f)) }

// OAuthCode is an error code of RFC 6749 sections 5.2 and 4.1.2.1, RFC
// 6750 section 3.1, or RFC 8628 section 3.5. A provider's code is kept only
// when it is one of these.
type OAuthCode uint8

const (
	CodeNone OAuthCode = iota
	CodeUnknown
	CodeInvalidRequest
	CodeInvalidClient
	CodeInvalidGrant
	CodeUnauthorizedClient
	CodeUnsupportedGrantType
	CodeInvalidScope
	CodeAccessDenied
	CodeUnsupportedResponseType
	CodeServerError
	CodeTemporarilyUnavailable
	CodeInvalidToken
	CodeInsufficientScope
	CodeAuthorizationPending
	CodeSlowDown
	CodeExpiredToken
)

var codeNames = [...]string{
	CodeNone: "no error code", CodeUnknown: "an error code writ does not know",
	CodeInvalidRequest: "invalid_request", CodeInvalidClient: "invalid_client", CodeInvalidGrant: "invalid_grant",
	CodeUnauthorizedClient: "unauthorized_client", CodeUnsupportedGrantType: "unsupported_grant_type",
	CodeInvalidScope: "invalid_scope", CodeAccessDenied: "access_denied", CodeUnsupportedResponseType: "unsupported_response_type",
	CodeServerError: "server_error", CodeTemporarilyUnavailable: "temporarily_unavailable", CodeInvalidToken: "invalid_token",
	CodeInsufficientScope: "insufficient_scope", CodeAuthorizationPending: "authorization_pending", CodeSlowDown: "slow_down",
	CodeExpiredToken: "expired_token",
}

func (c OAuthCode) String() string { return lookup(codeNames[:], int(c)) }

// parseOAuthCode keeps a provider's error code only when it is one Writ
// knows. The comparison is on the whole value, so no part of an unknown
// code survives.
func parseOAuthCode(s string) OAuthCode {
	if s == "" {
		return CodeNone
	}
	for c := CodeInvalidRequest; int(c) < len(codeNames); c++ {
		if codeNames[c] == s {
			return c
		}
	}
	return CodeUnknown
}

// Reason is why a stage refused.
type Reason uint8

const (
	// An endpoint's answer, or its absence.
	ReasonCanceled Reason = iota + 1
	ReasonDeadline
	ReasonOAuthError
	ReasonUnresolved
	ReasonUntrustedTLS
	ReasonTransportTimeout
	ReasonConnectFailed
	ReasonTransportFailed
	ReasonRedirect
	ReasonStatus
	ReasonTooLarge
	ReasonUnreadable
	ReasonNotSent
	ReasonNotJSON

	// Discovery.
	ReasonIssuerMismatch
	ReasonNotHTTPSOnIssuerHost
	ReasonNotHTTPS

	// A key set.
	ReasonTooManyKeys
	ReasonKeyNotJWK
	ReasonKidTwice
	ReasonRSAModulusLong
	ReasonRSAExponent
	ReasonKeyUnparsable
	ReasonPrivateKey
	ReasonRSABits
	ReasonRSAEven
	ReasonECCurve
	ReasonKeyType

	// A token's shape and header.
	ReasonTokenTooLong
	ReasonEncrypted
	ReasonParts
	ReasonNotBase64
	ReasonNotObject
	ReasonMemberTwice
	ReasonTooManyMembers
	ReasonForbiddenHeader
	ReasonMissing
	ReasonNotString
	ReasonEmpty
	ReasonAlg
	ReasonKidLength
	ReasonNotJWS
	ReasonNotCompactJWS

	// A token's issuer and signature.
	ReasonUntrustedIssuer
	ReasonOtherIssuer
	ReasonNoGoodKeySet
	ReasonUnknownKid
	ReasonKeyAlg
	ReasonSignature

	// A token's times, audience, and claims.
	ReasonNotNumber
	ReasonNotDate
	ReasonExpired
	ReasonIssuedInFuture
	ReasonNotYetValid
	ReasonAudOther
	ReasonAudNotStrings
	ReasonAudList
	ReasonAudNoClient
	ReasonAzpMissing
	ReasonAzpOther
	ReasonClaimMissing
	ReasonRefProtected
	ReasonLibrary
	ReasonNonce
	ReasonUserinfoSubject

	// The token endpoint and the device flow.
	ReasonNoIDToken
	ReasonNoDeviceFlow
	ReasonDenied
	ReasonCodeExpired
	ReasonUnusable

	// Writ's own config.
	ReasonIssuerURL
	ReasonNoClient
	ReasonPeopleIssuer
	ReasonDuplicateURL
)

// Stage is where the refusal happened.
func (r *Refusal) Stage() Stage { return r.stage }

// Check is the token check that failed, or 0 when the refusal is not a
// token's.
func (r *Refusal) Check() Check { return r.check }

// Reason is why the stage refused.
func (r *Refusal) Reason() Reason { return r.reason }

// Name is the check's name when a token failed a check, and the stage's
// otherwise, for an audit row's check column.
func (r *Refusal) Name() string {
	if r.check != 0 {
		return r.check.String()
	}
	return r.stage.String()
}

// Message says, in Writ's words, which stage refused and why: such as
// "token endpoint: status 400, invalid_grant" or "header: the token's alg
// is not RS256 or ES256".
func (r *Refusal) Message() string {
	why := r.why()
	switch r.reason {
	case ReasonDenied, ReasonCodeExpired, ReasonNoDeviceFlow:
		return why
	}
	if r.stage == StageConfig {
		return why
	}
	return r.Name() + ": " + why
}

func (r *Refusal) why() string {
	f := r.field.Name()
	key := "key " + strconv.Itoa(r.key) + " of the key set"
	switch r.reason {
	case ReasonCanceled:
		return "the request was cancelled"
	case ReasonDeadline:
		return "the request ran out of time"
	case ReasonOAuthError:
		if r.status == 0 {
			return r.code.String()
		}
		return "status " + strconv.Itoa(r.status) + ", " + r.code.String()
	case ReasonUnresolved:
		return "the host name does not resolve"
	case ReasonUntrustedTLS:
		return "the TLS certificate is not trusted"
	case ReasonTransportTimeout:
		return "the request timed out"
	case ReasonConnectFailed:
		return "the connection was refused or failed"
	case ReasonTransportFailed:
		return "the request failed before an answer arrived"
	case ReasonRedirect:
		return "status " + strconv.Itoa(r.status) + ", a redirect, which writ refuses"
	case ReasonStatus:
		return "status " + strconv.Itoa(r.status)
	case ReasonTooLarge:
		return "the answer is over " + strconv.FormatInt(r.limit, 10) + " bytes"
	case ReasonUnreadable:
		return "status " + strconv.Itoa(r.status) + ", an answer writ cannot read"
	case ReasonNotSent:
		return "the request could not be sent"
	case ReasonNotJSON:
		return "the answer is not the JSON object writ expects"
	case ReasonIssuerMismatch:
		return "the discovery document names another issuer than the configured URL"
	case ReasonNotHTTPSOnIssuerHost:
		return f + " is not an HTTPS URL on the issuer's host"
	case ReasonNotHTTPS:
		return "the discovery document's " + f + " is not an HTTPS URL"
	case ReasonTooManyKeys:
		return "the key set has " + strconv.FormatInt(r.count, 10) + " keys, over the limit of " + strconv.FormatInt(r.limit, 10)
	case ReasonKeyNotJWK:
		return key + " is not a JWK object"
	case ReasonKidTwice:
		return key + " has the kid of an earlier key, so a token could pick either"
	case ReasonRSAModulusLong:
		return key + ": the RSA modulus is too long"
	case ReasonRSAExponent:
		return key + ": the RSA exponent must be 65537"
	case ReasonKeyUnparsable:
		return key + " is not a JWK that go-jose can parse"
	case ReasonPrivateKey:
		return key + " is a private key"
	case ReasonRSABits:
		return key + ": the RSA modulus has " + strconv.FormatInt(r.count, 10) + " bits; writ accepts " +
			strconv.Itoa(minRSABits) + " to " + strconv.Itoa(maxRSABits)
	case ReasonRSAEven:
		return key + ": the RSA modulus is even"
	case ReasonECCurve:
		return key + ": the EC key is not on P-256"
	case ReasonKeyType:
		return key + " is not an RSA or a P-256 public key"
	case ReasonTokenTooLong:
		return "the token is " + strconv.FormatInt(r.count, 10) + " bytes, over the limit of " + strconv.FormatInt(r.limit, 10)
	case ReasonEncrypted:
		return "the token has five parts, an encrypted JWT, which writ does not accept"
	case ReasonParts:
		return "the token has " + strconv.FormatInt(r.count, 10) + " parts; a signed JWT has three"
	case ReasonNotBase64:
		return "the " + f + " is not base64url without padding"
	case ReasonNotObject:
		return "the " + f + " is not one JSON object"
	case ReasonMemberTwice:
		return "a member name appears twice in the " + f
	case ReasonTooManyMembers:
		return "the " + f + " has more than " + strconv.Itoa(maxMembers) + " members"
	case ReasonForbiddenHeader:
		return "the header has " + f + ", which writ refuses: a token may not name its own key or change how it is read"
	case ReasonMissing:
		return f + " is missing"
	case ReasonNotString:
		return f + " is not a string"
	case ReasonEmpty:
		return f + " is empty"
	case ReasonAlg:
		return "the token's alg is not RS256 or ES256"
	case ReasonKidLength:
		return "kid must be 1 to 256 characters"
	case ReasonNotJWS:
		return "the token is not a JWS writ can read"
	case ReasonNotCompactJWS:
		return "the token is not a compact JWS with one signature"
	case ReasonUntrustedIssuer:
		return "iss names no issuer writ serve trusts"
	case ReasonOtherIssuer:
		return "the ID token's iss is not the issuer's URL"
	case ReasonNoGoodKeySet:
		return "the issuer has had no good key set for " + MaxKeySetAge.String()
	case ReasonUnknownKid:
		return "the issuer's key set has no key with the token's kid"
	case ReasonKeyAlg:
		return "the key the token's kid names is not for the token's alg"
	case ReasonSignature:
		return "the signature does not match the key the token's kid names"
	case ReasonNotNumber:
		return f + " is not a number"
	case ReasonNotDate:
		return f + " is not a date in seconds since 1970"
	case ReasonExpired:
		return "the token has expired: its exp is past"
	case ReasonIssuedInFuture:
		return "the token's iat is in the future"
	case ReasonNotYetValid:
		return "the token's nbf is in the future"
	case ReasonAudOther:
		return "aud is not writ serve's URL"
	case ReasonAudNotStrings:
		return "aud is neither a string nor a list of strings"
	case ReasonAudList:
		return "aud must name only writ serve's URL; it names " + strconv.FormatInt(r.count, 10) + " audiences"
	case ReasonAudNoClient:
		return "aud does not name the issuer's client id"
	case ReasonAzpMissing:
		return "aud names " + strconv.FormatInt(r.count, 10) + " audiences and azp is missing"
	case ReasonAzpOther:
		return "azp is not the issuer's client id"
	case ReasonClaimMissing:
		return "claim " + f + " is missing; a token of the issuer's kind carries it"
	case ReasonRefProtected:
		return "claim ref_protected is not true or false"
	case ReasonLibrary:
		return "go-oidc refused a token that writ's own checks passed"
	case ReasonNonce:
		return "the ID token's nonce is not the one this sign-in sent"
	case ReasonUserinfoSubject:
		return "userinfo names another subject than the ID token"
	case ReasonNoIDToken:
		return "the answer holds no id_token"
	case ReasonNoDeviceFlow:
		return "the identity provider's discovery document names no device_authorization_endpoint, so it offers no device flow"
	case ReasonDenied:
		return "the person refused the sign-in at the identity provider"
	case ReasonCodeExpired:
		return "the sign-in code ran out before the person signed in"
	case ReasonUnusable:
		return "the answer holds no usable " + f
	case ReasonIssuerURL:
		return "url must be an HTTPS URL with no user, query, or fragment"
	case ReasonNoClient:
		return "a people issuer needs a client id and a client secret"
	case ReasonPeopleIssuer:
		return "a people issuer signs people in through NewProvider, not Verify"
	case ReasonDuplicateURL:
		return "another issuer has the same URL"
	}
	return "refused for a reason writ does not name"
}

// String is the message, so a refusal printed by accident still says only
// Writ's words.
func (r *Refusal) String() string { return r.Message() }

func lookup(names []string, i int) string {
	if i <= 0 || i >= len(names) || names[i] == "" {
		return "unknown"
	}
	return names[i]
}

func refuse(stage Stage, reason Reason) *Refusal { return &Refusal{stage: stage, reason: reason} }

// failed is a token that failed check for reason.
func failed(check Check, reason Reason) *Refusal {
	return &Refusal{stage: StageToken, check: check, reason: reason}
}

func (r *Refusal) of(f Field) *Refusal { r.field = f; return r }

func (r *Refusal) counted(count, limit int64) *Refusal { r.count, r.limit = count, limit; return r }

func (r *Refusal) atKey(i int) *Refusal { r.key = i; return r }

// CallbackRefusal is the browser's return from the authorization endpoint
// with an error parameter: the code when it is one Writ knows.
func CallbackRefusal(errorParam string) *Refusal {
	r := refuse(StageAuthorization, ReasonOAuthError)
	r.code = parseOAuthCode(errorParam)
	return r
}
