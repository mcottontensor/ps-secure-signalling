package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
)

// StaticToken is one shared secret and the identity it grants.
type StaticToken struct {
	Token     string
	Subject   string
	Roles     []Role
	Streamers []string
	PublishAs []string
}

// StaticTokens authenticates pre-shared bearer tokens. Comparison is constant time and every
// configured token is checked, so timing reveals neither which token matched nor how much of one.
type StaticTokens struct {
	Source TokenSource
	tokens []StaticToken
	hashes [][32]byte
}

// NewStaticTokens builds the authenticator. Tokens shorter than 16 bytes are refused, as they are
// guessable.
func NewStaticTokens(src TokenSource, tokens []StaticToken) (*StaticTokens, error) {
	s := &StaticTokens{Source: src}
	for _, t := range tokens {
		if len(t.Token) < 16 {
			return nil, errors.New("auth: static tokens must be at least 16 characters")
		}
		if len(t.Roles) == 0 {
			return nil, errors.New("auth: static token " + t.Subject + " grants no roles")
		}
		s.tokens = append(s.tokens, t)
		s.hashes = append(s.hashes, sha256.Sum256([]byte(t.Token)))
	}
	return s, nil
}

func (s *StaticTokens) Authenticate(_ context.Context, r *http.Request, role Role) (*Identity, error) {
	presented := s.Source.Tokens(r)
	if len(presented) == 0 {
		return nil, ErrNoCredentials
	}
	match := -1
	for _, p := range presented {
		h := sha256.Sum256([]byte(p))
		for i := range s.hashes {
			if subtle.ConstantTimeCompare(h[:], s.hashes[i][:]) == 1 && match < 0 {
				match = i
			}
		}
	}
	if match < 0 {
		// Not necessarily wrong: the token may be a JWT meant for another authenticator.
		return nil, fmt.Errorf("%w: unknown token", ErrUnauthorized)
	}
	t := s.tokens[match]
	subject := t.Subject
	if subject == "" {
		subject = "token"
	}
	return &Identity{
		Subject:   subject,
		Method:    "token",
		Roles:     t.Roles,
		Streamers: t.Streamers,
		PublishAs: t.PublishAs,
	}, nil
}
