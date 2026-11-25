package keycloak

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

//go:generate mockgen -source=client.go -destination=mocks/mock_client.go -package=mocks
type KeycloakClient interface {
	AssignClientRole(ctx context.Context, keycloakUserID string, clientID string, roleName string) error
	RevokeClientRole(ctx context.Context, keycloakUserID string, clientID string, roleName string) error
	CreateUser(ctx context.Context, input CreateUserInput) (*UserProfile, error)
	GetUserByID(ctx context.Context, keycloakUserID string) (*UserProfile, error)
	FindUserByEmail(ctx context.Context, email string) (*UserProfile, error)

	// Annuaire derrière le backend (directory.go).
	VerifyPassword(ctx context.Context, email, password string) (*PasswordCheck, error)
	MarkEmailVerified(ctx context.Context, subject string) error
	SetPassword(ctx context.Context, subject, password string) error
	HasPassword(ctx context.Context, subject string) (bool, error)
}

var (
	ErrUserNotFound       = errors.New("keycloak user not found")
	ErrUserAlreadyExists  = errors.New("keycloak user already exists")
	ErrInvalidCredentials = errors.New("invalid credentials")
)

type userTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

type AdminConfig struct {
	BaseURL      string
	Realm        string
	ClientID     string
	ClientSecret string
	// AuthClientID / AuthClientSecret : client confidentiel de vérification
	// des mots de passe (VerifyPassword). Vide : vérification indisponible.
	AuthClientID     string
	AuthClientSecret string
	Timeout          time.Duration
}

type Client struct {
	baseURL      string
	realm        string
	clientID     string
	clientSecret string
	pwdClientID  string
	pwdSecret    string
	httpClient   *http.Client

	mu             sync.Mutex
	accessToken    string
	accessTokenExp time.Time
	clientUUIDs    map[string]string
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
}

type clientRepresentation struct {
	ID       string `json:"id"`
	ClientID string `json:"clientId"`
}

type roleRepresentation struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type credentialRepresentation struct {
	Type      string `json:"type"`
	Value     string `json:"value"`
	Temporary bool   `json:"temporary"`
}

type userRepresentation struct {
	ID            string                     `json:"id,omitempty"`
	Username      string                     `json:"username,omitempty"`
	Email         string                     `json:"email,omitempty"`
	FirstName     string                     `json:"firstName,omitempty"`
	LastName      string                     `json:"lastName,omitempty"`
	EmailVerified bool                       `json:"emailVerified,omitempty"`
	Enabled       bool                       `json:"enabled,omitempty"`
	Attributes    map[string][]string        `json:"attributes,omitempty"`
	Credentials   []credentialRepresentation `json:"credentials,omitempty"`
}

type CreateUserInput struct {
	Email    string
	Password string
	// WithoutPassword : compte créé par une connexion Google/GitHub, sans
	// mot de passe (la personne pourra en définir un plus tard).
	WithoutPassword bool
	FullName        string
	PhoneNumber     string
	// Country : pays donné à l'inscription (code déjà validé), gardé dans
	// l'annuaire jusqu'à la première connexion qui crée le profil local.
	Country       string
	Enabled       bool
	EmailVerified bool
}

type UserProfile struct {
	ID          string
	Email       string
	FullName    string
	PhoneNumber string
	// Country : attribut « country » posé à l'inscription ("" : aucun).
	Country       string
	AvatarURL     string
	EmailVerified bool
	Enabled       bool
}

func New(cfg AdminConfig, issuerURL string) (*Client, error) {
	baseURL := strings.TrimSpace(cfg.BaseURL)
	realm := strings.TrimSpace(cfg.Realm)
	if baseURL == "" || realm == "" {
		derivedBaseURL, derivedRealm, err := deriveBaseURLAndRealm(issuerURL)
		if err != nil {
			return nil, err
		}
		if baseURL == "" {
			baseURL = derivedBaseURL
		}
		if realm == "" {
			realm = derivedRealm
		}
	}
	clientID := strings.TrimSpace(cfg.ClientID)
	clientSecret := strings.TrimSpace(cfg.ClientSecret)
	if baseURL == "" || realm == "" || clientID == "" || clientSecret == "" {
		return nil, errors.New("incomplete keycloak admin configuration")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Client{
		baseURL:      strings.TrimRight(baseURL, "/"),
		realm:        realm,
		clientID:     clientID,
		clientSecret: clientSecret,
		pwdClientID:  strings.TrimSpace(cfg.AuthClientID),
		pwdSecret:    strings.TrimSpace(cfg.AuthClientSecret),
		httpClient:   &http.Client{Timeout: timeout},
		clientUUIDs:  make(map[string]string),
	}, nil
}

func (c *Client) AssignClientRole(ctx context.Context, keycloakUserID string, clientID string, roleName string) error {
	keycloakUserID = strings.TrimSpace(keycloakUserID)
	clientID = strings.TrimSpace(clientID)
	roleName = strings.TrimSpace(roleName)
	if keycloakUserID == "" || clientID == "" || roleName == "" {
		return errors.New("keycloak user id, client id and role name are required")
	}
	token, err := c.adminAccessToken(ctx)
	if err != nil {
		return err
	}
	clientUUID, err := c.clientUUID(ctx, token, clientID)
	if err != nil {
		return err
	}
	role, err := c.clientRole(ctx, token, clientUUID, roleName)
	if err != nil {
		return err
	}
	payload, err := json.Marshal([]roleRepresentation{role})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		fmt.Sprintf("%s/admin/realms/%s/users/%s/role-mappings/clients/%s", c.baseURL, url.PathEscape(c.realm), url.PathEscape(keycloakUserID), url.PathEscape(clientUUID)),
		bytes.NewReader(payload),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("assign keycloak role failed with status %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) RevokeClientRole(ctx context.Context, keycloakUserID string, clientID string, roleName string) error {
	keycloakUserID = strings.TrimSpace(keycloakUserID)
	clientID = strings.TrimSpace(clientID)
	roleName = strings.TrimSpace(roleName)
	if keycloakUserID == "" || clientID == "" || roleName == "" {
		return errors.New("keycloak user id, client id and role name are required")
	}
	token, err := c.adminAccessToken(ctx)
	if err != nil {
		return err
	}
	clientUUID, err := c.clientUUID(ctx, token, clientID)
	if err != nil {
		return err
	}
	role, err := c.clientRole(ctx, token, clientUUID, roleName)
	if err != nil {
		return err
	}
	payload, err := json.Marshal([]roleRepresentation{role})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodDelete,
		fmt.Sprintf("%s/admin/realms/%s/users/%s/role-mappings/clients/%s", c.baseURL, url.PathEscape(c.realm), url.PathEscape(keycloakUserID), url.PathEscape(clientUUID)),
		bytes.NewReader(payload),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("revoke keycloak role failed with status %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) CreateUser(ctx context.Context, input CreateUserInput) (*UserProfile, error) {
	email := strings.ToLower(strings.TrimSpace(input.Email))
	password := strings.TrimSpace(input.Password)
	if email == "" || (password == "" && !input.WithoutPassword) {
		return nil, errors.New("email and password are required")
	}
	fullName := strings.TrimSpace(input.FullName)
	phoneNumber := strings.TrimSpace(input.PhoneNumber)
	token, err := c.adminAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	firstName, lastName := splitFullName(fullName)
	payload := userRepresentation{
		Username:      email,
		Email:         email,
		FirstName:     firstName,
		LastName:      lastName,
		EmailVerified: input.EmailVerified,
		Enabled:       true,
	}
	if !input.WithoutPassword {
		payload.Credentials = []credentialRepresentation{{Type: "password", Value: password, Temporary: false}}
	}
	if !input.Enabled {
		payload.Enabled = false
	}
	attributes := buildAttributes(fullName, phoneNumber, strings.ToUpper(strings.TrimSpace(input.Country)))
	if len(attributes) > 0 {
		payload.Attributes = attributes
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		fmt.Sprintf("%s/admin/realms/%s/users", c.baseURL, url.PathEscape(c.realm)),
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusCreated:
		location := strings.TrimSpace(resp.Header.Get("Location"))
		if userID := lastPathSegment(location); userID != "" {
			return c.GetUserByID(ctx, userID)
		}
		return c.FindUserByEmail(ctx, email)
	case http.StatusConflict:
		return nil, ErrUserAlreadyExists
	default:
		return nil, fmt.Errorf("create keycloak user failed with status %d", resp.StatusCode)
	}
}

func (c *Client) GetUserByID(ctx context.Context, keycloakUserID string) (*UserProfile, error) {
	keycloakUserID = strings.TrimSpace(keycloakUserID)
	if keycloakUserID == "" {
		return nil, errors.New("keycloak user id is required")
	}
	token, err := c.adminAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		fmt.Sprintf("%s/admin/realms/%s/users/%s", c.baseURL, url.PathEscape(c.realm), url.PathEscape(keycloakUserID)),
		nil,
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrUserNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("keycloak user lookup failed with status %d", resp.StatusCode)
	}
	var user userRepresentation
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return nil, err
	}
	return mapUserProfile(user), nil
}

func (c *Client) FindUserByEmail(ctx context.Context, email string) (*UserProfile, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return nil, errors.New("email is required")
	}
	token, err := c.adminAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		fmt.Sprintf("%s/admin/realms/%s/users?email=%s&exact=true", c.baseURL, url.PathEscape(c.realm), url.QueryEscape(email)),
		nil,
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("keycloak user email lookup failed with status %d", resp.StatusCode)
	}
	var users []userRepresentation
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		return nil, err
	}
	for _, user := range users {
		if strings.EqualFold(strings.TrimSpace(user.Email), email) {
			return mapUserProfile(user), nil
		}
	}
	return nil, ErrUserNotFound
}

func (c *Client) adminAccessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.accessToken != "" && time.Until(c.accessTokenExp) > 15*time.Second {
		token := c.accessToken
		c.mu.Unlock()
		return token, nil
	}
	c.mu.Unlock()

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", c.clientID)
	form.Set("client_secret", c.clientSecret)

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		fmt.Sprintf("%s/realms/%s/protocol/openid-connect/token", c.baseURL, url.PathEscape(c.realm)),
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("keycloak token request failed with status %d", resp.StatusCode)
	}
	var tokenResp tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", err
	}
	if strings.TrimSpace(tokenResp.AccessToken) == "" {
		return "", errors.New("keycloak token response missing access_token")
	}
	c.mu.Lock()
	c.accessToken = tokenResp.AccessToken
	c.accessTokenExp = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	c.mu.Unlock()
	return tokenResp.AccessToken, nil
}

func (c *Client) clientUUID(ctx context.Context, token string, clientID string) (string, error) {
	c.mu.Lock()
	if clientUUID, ok := c.clientUUIDs[clientID]; ok && clientUUID != "" {
		c.mu.Unlock()
		return clientUUID, nil
	}
	c.mu.Unlock()

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		fmt.Sprintf("%s/admin/realms/%s/clients?clientId=%s", c.baseURL, url.PathEscape(c.realm), url.QueryEscape(clientID)),
		nil,
	)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("keycloak client lookup failed with status %d", resp.StatusCode)
	}
	var clients []clientRepresentation
	if err := json.NewDecoder(resp.Body).Decode(&clients); err != nil {
		return "", err
	}
	for _, client := range clients {
		if client.ClientID != clientID {
			continue
		}
		c.mu.Lock()
		c.clientUUIDs[clientID] = client.ID
		c.mu.Unlock()
		return client.ID, nil
	}
	return "", fmt.Errorf("keycloak client %s not found", clientID)
}

func (c *Client) clientRole(ctx context.Context, token string, clientUUID string, roleName string) (roleRepresentation, error) {
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		fmt.Sprintf("%s/admin/realms/%s/clients/%s/roles/%s", c.baseURL, url.PathEscape(c.realm), url.PathEscape(clientUUID), url.PathEscape(roleName)),
		nil,
	)
	if err != nil {
		return roleRepresentation{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return roleRepresentation{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return roleRepresentation{}, fmt.Errorf("keycloak role lookup failed with status %d", resp.StatusCode)
	}
	var role roleRepresentation
	if err := json.NewDecoder(resp.Body).Decode(&role); err != nil {
		return roleRepresentation{}, err
	}
	if strings.TrimSpace(role.ID) == "" {
		return roleRepresentation{}, fmt.Errorf("keycloak role %s not found", roleName)
	}
	return role, nil
}

func deriveBaseURLAndRealm(issuerURL string) (string, string, error) {
	issuerURL = strings.TrimRight(strings.TrimSpace(issuerURL), "/")
	if issuerURL == "" {
		return "", "", errors.New("issuer url is required")
	}
	idx := strings.LastIndex(issuerURL, "/realms/")
	if idx < 0 {
		return "", "", fmt.Errorf("issuer url %s does not contain /realms/", issuerURL)
	}
	baseURL := issuerURL[:idx]
	realm := issuerURL[idx+len("/realms/"):]
	if baseURL == "" || strings.TrimSpace(realm) == "" {
		return "", "", fmt.Errorf("issuer url %s is invalid", issuerURL)
	}
	return baseURL, realm, nil
}

func mapUserProfile(user userRepresentation) *UserProfile {
	if strings.TrimSpace(user.ID) == "" {
		return nil
	}
	fullName := firstAttributeValue(user.Attributes, "full_name")
	if fullName == "" {
		fullName = strings.TrimSpace(strings.TrimSpace(user.FirstName + " " + user.LastName))
	}
	avatarURL := firstAttributeValue(user.Attributes, "picture")
	if avatarURL == "" {
		avatarURL = firstAttributeValue(user.Attributes, "avatar_url")
	}
	return &UserProfile{
		ID:            strings.TrimSpace(user.ID),
		Email:         strings.ToLower(strings.TrimSpace(user.Email)),
		FullName:      fullName,
		PhoneNumber:   firstAttributeValue(user.Attributes, "phone_number"),
		Country:       strings.ToUpper(firstAttributeValue(user.Attributes, "country")),
		AvatarURL:     avatarURL,
		EmailVerified: user.EmailVerified,
		Enabled:       user.Enabled,
	}
}

// buildAttributes : attributs posés à la création. « country » est déclaré
// dans le profil utilisateur du realm (scripts/keycloak/bootstrap_platform.sh,
// ensure_user_profile) ; full_name et phone_number ne le sont pas : Keycloak
// 24+ les ignore sans erreur (attributs non gérés).
func buildAttributes(fullName, phoneNumber, country string) map[string][]string {
	attributes := map[string][]string{}
	if fullName != "" {
		attributes["full_name"] = []string{fullName}
	}
	if phoneNumber != "" {
		attributes["phone_number"] = []string{phoneNumber}
	}
	if country != "" {
		attributes["country"] = []string{country}
	}
	if len(attributes) == 0 {
		return nil
	}
	return attributes
}

func firstAttributeValue(attributes map[string][]string, key string) string {
	values := attributes[key]
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}

func splitFullName(fullName string) (string, string) {
	parts := strings.Fields(strings.TrimSpace(fullName))
	switch len(parts) {
	case 0:
		return "", ""
	case 1:
		return parts[0], ""
	default:
		return parts[0], strings.Join(parts[1:], " ")
	}
}

func lastPathSegment(value string) string {
	value = strings.TrimSpace(strings.TrimRight(value, "/"))
	if value == "" {
		return ""
	}
	if idx := strings.LastIndexByte(value, '/'); idx >= 0 && idx < len(value)-1 {
		return value[idx+1:]
	}
	return value
}
