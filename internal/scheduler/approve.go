package scheduler

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
)

// ActionSig signs an (action, entity id, doc url) triple for the portal's
// one-tap ntfy endpoints. Key = the Homebox API token (already shared by both
// processes via config), so a crafted URL without it cannot attach or reject
// anything. The action verb is part of the MAC so an approve link cannot be
// replayed against the reject endpoint or vice versa.
func ActionSig(action, entityID, docURL, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(action + "\x00" + entityID + "\x00" + docURL))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// ActionURL builds a signed portal action link (action = "approve" | "reject").
func ActionURL(portalBase, action, entityID, docURL, key string) string {
	q := url.Values{}
	q.Set("id", entityID)
	q.Set("url", docURL)
	q.Set("sig", ActionSig(action, entityID, docURL, key))
	return fmt.Sprintf("%s/api/%s?%s", portalBase, action, q.Encode())
}
