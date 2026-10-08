// Package history signs assistant replies so a stateless server can tell its
// own words from ones a client wrote into the history it sends back.
package history

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"hash"

	chatpb "chat-service/chat"
)

// layoutVersion is hashed first, so a future layout can never verify as this one.
const layoutVersion = "chat-history-v1"

type Signer struct {
	key []byte
}

// NewSigner draws a per-process key. A restart invalidates every signature,
// which only costs open conversations their earlier context.
func NewSigner() *Signer {
	key := make([]byte, 32)
	rand.Read(key) // crashes the program rather than returning an error since Go 1.24
	return &Signer{key: key}
}

// Sign binds a reply to the question it answered.
func (s *Signer) Sign(question, reply string) []byte {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(layoutVersion))
	writeField(mac, question)
	writeField(mac, reply)
	return mac.Sum(nil)
}

func writeField(mac hash.Hash, field string) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(field)))
	mac.Write(n[:])
	mac.Write([]byte(field))
}

// Verify keeps every user turn and each assistant turn whose signature matches
// the user turn right before it. Dropping rather than rejecting: a reply the
// visitor stopped mid-stream never got a signature.
func (s *Signer) Verify(msgs []*chatpb.Message) ([]*chatpb.Message, map[string]int) {
	kept := make([]*chatpb.Message, 0, len(msgs))
	dropped := map[string]int{}
	for i, m := range msgs {
		if m.GetRole() != chatpb.Role_ROLE_ASSISTANT {
			kept = append(kept, m)
			continue
		}
		switch {
		case len(m.GetSignature()) == 0:
			dropped["missing"]++
		case i == 0 || msgs[i-1].GetRole() != chatpb.Role_ROLE_USER,
			!hmac.Equal(s.Sign(msgs[i-1].GetContent(), m.GetContent()), m.GetSignature()):
			dropped["invalid"]++
		default:
			kept = append(kept, m)
		}
	}
	return kept, dropped
}
