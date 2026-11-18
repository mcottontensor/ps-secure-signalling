// Package turn mints time-limited TURN credentials in the "TURN REST API" scheme that coturn
// implements as use-auth-secret / static-auth-secret: the username is "<expiry>:<user>" and the
// password is base64(HMAC-SHA1(secret, username)). The TURN server checks both without having to
// be told about each credential, and a leaked one stops working at its expiry.
package turn

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Credentials returns a username and credential valid until now+ttl.
func Credentials(secret []byte, ttl time.Duration, user string, now time.Time) (username, credential string) {
	username = strconv.FormatInt(now.Add(ttl).Unix(), 10) + ":" + user
	mac := hmac.New(sha1.New, secret)
	mac.Write([]byte(username))
	return username, base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// Apply returns a copy of peerOptions (an RTCConfiguration-shaped object) in which every ICE
// server with a turn: or turns: URL carries fresh credentials for user. Other entries, and
// anything that is not shaped as expected, pass through unchanged.
func Apply(peerOptions json.RawMessage, secret []byte, ttl time.Duration, user string) (json.RawMessage, error) {
	var opts map[string]json.RawMessage
	if err := json.Unmarshal(peerOptions, &opts); err != nil {
		return nil, err
	}
	var servers []map[string]any
	if raw, ok := opts["iceServers"]; !ok || json.Unmarshal(raw, &servers) != nil {
		return peerOptions, nil
	}
	username, credential := Credentials(secret, ttl, user, time.Now())
	for _, srv := range servers {
		if isTURN(srv) {
			srv["username"], srv["credential"] = username, credential
		}
	}
	raw, err := json.Marshal(servers)
	if err != nil {
		return nil, err
	}
	opts["iceServers"] = raw
	return json.Marshal(opts)
}

func isTURN(srv map[string]any) bool {
	urls := srv["urls"]
	if urls == nil {
		urls = srv["url"]
	}
	list, ok := urls.([]any)
	if !ok {
		list = []any{urls}
	}
	for _, u := range list {
		if s, ok := u.(string); ok {
			s = strings.ToLower(s)
			if strings.HasPrefix(s, "turn:") || strings.HasPrefix(s, "turns:") {
				return true
			}
		}
	}
	return false
}
