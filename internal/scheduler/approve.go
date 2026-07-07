package scheduler

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
)

// ApproveSig signs an (entity id, doc url) pair for the portal's one-tap
// approve endpoint. Key = the Homebox API token (already shared by both
// processes via config), so a crafted URL without it cannot attach anything.
func ApproveSig(entityID, docURL, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(entityID + "\x00" + docURL))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// ApproveURL builds the portal approve link for a review-gated candidate.
func ApproveURL(portalBase, entityID, docURL, key string) string {
	q := url.Values{}
	q.Set("id", entityID)
	q.Set("url", docURL)
	q.Set("sig", ApproveSig(entityID, docURL, key))
	return fmt.Sprintf("%s/api/approve?%s", portalBase, q.Encode())
}
