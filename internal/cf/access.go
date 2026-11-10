package cf

import (
	"context"
	"encoding/json"
	"net/http"
)

// AccessSpec describes a self-hosted Access application with a single
// "allow" policy for the given emails and email domains.
type AccessSpec struct {
	Name            string
	Domain          string
	SessionDuration string
	Emails          []string
	EmailDomains    []string
}

func (s AccessSpec) appBody() map[string]any {
	return map[string]any{
		"type":                 "self_hosted",
		"name":                 s.Name,
		"domain":               s.Domain,
		"destinations":         []map[string]string{{"type": "public", "uri": s.Domain}},
		"session_duration":     s.SessionDuration,
		"app_launcher_visible": false,
	}
}

func (s AccessSpec) policyBody() map[string]any {
	include := []map[string]any{}
	for _, e := range s.Emails {
		include = append(include, map[string]any{"email": map[string]string{"email": e}})
	}
	for _, d := range s.EmailDomains {
		include = append(include, map[string]any{"email_domain": map[string]string{"domain": d}})
	}
	return map[string]any{"name": "Allowed", "decision": "allow", "precedence": 1, "include": include}
}

type idOnly struct {
	ID string `json:"id"`
}

// EnsureAccessApp creates or updates the application and its policy. The app
// is written before the policy: an Access app without policies denies
// everyone, so a partial failure never leaves the host open. The ids obtained
// so far are returned even on error.
func (c *Client) EnsureAccessApp(ctx context.Context, accountID, appID, policyID string, spec AccessSpec) (string, string, error) {
	apps := "/accounts/" + accountID + "/access/apps"
	var app idOnly
	if appID != "" {
		_, err := c.do(ctx, http.MethodPut, apps+"/"+appID, nil, spec.appBody(), &app)
		if IsNotFound(err) {
			appID, policyID = "", "" // removed from the dashboard: recreate
		} else if err != nil {
			return appID, policyID, err
		}
	}
	if appID == "" {
		if _, err := c.do(ctx, http.MethodPost, apps, nil, spec.appBody(), &app); err != nil {
			return "", "", err
		}
		appID = app.ID
	}
	policies := apps + "/" + appID + "/policies"
	var pol idOnly
	if policyID != "" {
		_, err := c.do(ctx, http.MethodPut, policies+"/"+policyID, nil, spec.policyBody(), &pol)
		if err == nil {
			return appID, policyID, nil
		}
		if !IsNotFound(err) {
			return appID, policyID, err
		}
	}
	if _, err := c.do(ctx, http.MethodPost, policies, nil, spec.policyBody(), &pol); err != nil {
		return appID, "", err
	}
	return appID, pol.ID, nil
}

func (c *Client) DeleteAccessApp(ctx context.Context, accountID, appID string) error {
	_, err := c.do(ctx, http.MethodDelete, "/accounts/"+accountID+"/access/apps/"+appID, nil, nil, nil)
	return err
}

// AccessOrganization checks that Access is usable with this token by listing
// applications (which fails when Zero Trust is not set up or the token lacks
// the "Access: Apps and Policies" permission).
func (c *Client) AccessOrganization(ctx context.Context, accountID string) error {
	var raw json.RawMessage
	_, err := c.do(ctx, http.MethodGet, "/accounts/"+accountID+"/access/apps", nil, nil, &raw)
	return err
}
