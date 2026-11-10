package api

import (
	"context"
	"net/http"
	"net/url"

	bigav1 "github.com/biga-app/cli/internal/api/apipb/biga/v1"
)

type Extension struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"createdAt"`
}

type ExtensionGrant struct {
	Extension
	Token string `json:"token"`
}

func (c *Client) CreateExtension(ctx context.Context, sessionID, ownerToken, name string) (ExtensionGrant, error) {
	var grant bigav1.ExtensionGrant
	_, err := c.do(ctx, http.MethodPost, sessionPath(sessionID, "extensions"), &bigav1.CreateExtensionRequest{Name: name}, &grant, request{attempts: 1, token: ownerToken})
	if err != nil {
		return ExtensionGrant{}, err
	}
	return ExtensionGrant{Extension: extensionFromPB(grant.GetExtension()), Token: grant.GetToken()}, nil
}

func (c *Client) Extensions(ctx context.Context, sessionID, ownerToken string) ([]Extension, error) {
	var list bigav1.ListExtensionsResponse
	if _, err := c.do(ctx, http.MethodGet, sessionPath(sessionID, "extensions"), nil, &list, request{attempts: 2, token: ownerToken}); err != nil {
		return nil, err
	}
	extensions := make([]Extension, len(list.GetExtensions()))
	for i, e := range list.GetExtensions() {
		extensions[i] = extensionFromPB(e)
	}
	return extensions, nil
}

func (c *Client) RevokeExtension(ctx context.Context, sessionID, ownerToken, id string) error {
	_, err := c.do(ctx, http.MethodDelete, sessionPath(sessionID, "extensions/"+url.PathEscape(id)), nil, nil, request{attempts: 2, token: ownerToken})
	return err
}
