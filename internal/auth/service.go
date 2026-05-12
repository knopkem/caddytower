package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"caddytower/internal/secrets"
	"caddytower/internal/store"

	"github.com/google/uuid"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookieName = "caddytower_session"
	csrfCookieName    = "caddytower_csrf"
	defaultIssuer     = "CaddyTower"
)

var (
	ErrInvalidCredentials = errors.New("invalid email, password, or TOTP code")
	ErrAccountLocked      = errors.New("account temporarily locked")
	ErrTooManyRequests    = errors.New("too many login attempts from this IP")
	ErrSetupComplete      = errors.New("initial admin already exists")
	ErrInvalidSetup       = errors.New("invalid setup submission")
)

type Service struct {
	store           *store.Store
	secrets         *secrets.Service
	publicBaseURL   string
	now             func() time.Time
	sessionTTL      time.Duration
	lockoutAfter    int
	lockoutDuration time.Duration
	ipWindow        time.Duration
	ipLimit         int
	ipMu            sync.Mutex
	ipAttempts      map[string][]time.Time
}

type User struct {
	ID        string
	Email     string
	CreatedAt time.Time
}

type Enrollment struct {
	Secret    string
	URL       string
	ManualKey string
}

func New(stateStore *store.Store, secretService *secrets.Service, publicBaseURL string) *Service {
	return &Service{
		store:           stateStore,
		secrets:         secretService,
		publicBaseURL:   publicBaseURL,
		now:             time.Now,
		sessionTTL:      12 * time.Hour,
		lockoutAfter:    5,
		lockoutDuration: 15 * time.Minute,
		ipWindow:        5 * time.Minute,
		ipLimit:         20,
		ipAttempts:      map[string][]time.Time{},
	}
}

func (s *Service) SetNow(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}

func (s *Service) BootstrapRequired(ctx context.Context) (bool, error) {
	count, err := s.store.UserCount(ctx)
	if err != nil {
		return false, err
	}
	return count == 0, nil
}

func (s *Service) GenerateEnrollment(email string) (*Enrollment, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" {
		return nil, fmt.Errorf("email must not be empty")
	}

	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      defaultIssuer,
		AccountName: email,
		Period:      30,
		SecretSize:  20,
	})
	if err != nil {
		return nil, fmt.Errorf("generate totp secret: %w", err)
	}

	return s.BuildEnrollment(email, key.Secret()), nil
}

func (s *Service) BuildEnrollment(email, secret string) *Enrollment {
	email = strings.TrimSpace(strings.ToLower(email))
	issuer := url.QueryEscape(defaultIssuer)
	account := url.PathEscape(defaultIssuer + ":" + email)
	rawSecret := strings.TrimSpace(secret)

	return &Enrollment{
		Secret:    rawSecret,
		URL:       fmt.Sprintf("otpauth://totp/%s?secret=%s&issuer=%s&period=30", account, url.QueryEscape(rawSecret), issuer),
		ManualKey: rawSecret,
	}
}

func (s *Service) CreateInitialUser(ctx context.Context, email, password, confirmPassword, totpSecret, code, ip, userAgent string) (string, User, error) {
	required, err := s.BootstrapRequired(ctx)
	if err != nil {
		return "", User{}, err
	}
	if !required {
		return "", User{}, ErrSetupComplete
	}

	email = strings.TrimSpace(strings.ToLower(email))
	totpSecret = strings.TrimSpace(totpSecret)
	code = strings.TrimSpace(code)
	if email == "" || password == "" || confirmPassword == "" || totpSecret == "" || code == "" {
		return "", User{}, ErrInvalidSetup
	}
	if password != confirmPassword {
		return "", User{}, ErrInvalidSetup
	}
	if len(password) < 12 {
		return "", User{}, fmt.Errorf("password must be at least 12 characters")
	}

	if !s.validateTOTP(totpSecret, code, s.now()) {
		return "", User{}, ErrInvalidCredentials
	}

	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return "", User{}, fmt.Errorf("hash password: %w", err)
	}

	secretAtRest, err := s.encodeSecret(totpSecret)
	if err != nil {
		return "", User{}, err
	}

	user := store.UserRecord{
		ID:           uuid.NewString(),
		Email:        email,
		PasswordHash: string(passwordHash),
		TOTPSecret:   secretAtRest,
	}

	if err := s.store.CreateUser(ctx, user); err != nil {
		return "", User{}, err
	}

	if err := s.store.InsertAuditLog(ctx, uuid.NewString(), user.ID, "user.bootstrap", "user:"+user.ID, map[string]any{
		"email": email,
		"ip":    ip,
	}); err != nil {
		return "", User{}, err
	}

	token, err := s.createSession(ctx, user.ID, ip, userAgent)
	if err != nil {
		return "", User{}, err
	}

	return token, userFromRecord(user), nil
}

func (s *Service) Login(ctx context.Context, email, password, code, ip, userAgent string) (string, User, error) {
	if !s.allowIP(ip) {
		return "", User{}, ErrTooManyRequests
	}

	email = strings.TrimSpace(strings.ToLower(email))
	code = strings.TrimSpace(code)

	user, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", User{}, ErrInvalidCredentials
		}
		return "", User{}, err
	}

	now := s.now()
	if user.LockedUntil != nil && user.LockedUntil.After(now) {
		return "", User{}, ErrAccountLocked
	}

	secret, err := s.decodeSecret(user.TOTPSecret)
	if err != nil {
		return "", User{}, err
	}

	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)) != nil || !s.validateTOTP(secret, code, now) {
		failedCount := user.FailedLoginCount + 1
		var lockedUntil *time.Time
		if failedCount >= s.lockoutAfter {
			until := now.Add(s.lockoutDuration)
			lockedUntil = &until
		}

		if err := s.store.UpdateUserLoginFailures(ctx, user.ID, failedCount, lockedUntil); err != nil {
			return "", User{}, err
		}

		_ = s.store.InsertAuditLog(ctx, uuid.NewString(), user.ID, "user.login_failed", "user:"+user.ID, map[string]any{
			"email": email,
			"ip":    ip,
		})

		if lockedUntil != nil {
			return "", User{}, ErrAccountLocked
		}
		return "", User{}, ErrInvalidCredentials
	}

	if err := s.store.ClearUserLoginFailures(ctx, user.ID); err != nil {
		return "", User{}, err
	}

	token, err := s.createSession(ctx, user.ID, ip, userAgent)
	if err != nil {
		return "", User{}, err
	}

	s.clearIPAttempts(ip)

	if err := s.store.InsertAuditLog(ctx, uuid.NewString(), user.ID, "user.login", "user:"+user.ID, map[string]any{
		"email": email,
		"ip":    ip,
	}); err != nil {
		return "", User{}, err
	}

	return token, userFromRecord(user), nil
}

func (s *Service) Authenticate(ctx context.Context, rawToken string) (User, error) {
	if strings.TrimSpace(rawToken) == "" {
		return User{}, store.ErrNotFound
	}

	if err := s.store.DeleteExpiredSessions(ctx, s.now()); err != nil {
		return User{}, err
	}

	session, err := s.store.GetSession(ctx, hashToken(rawToken))
	if err != nil {
		return User{}, err
	}

	if session.ExpiresAt.Before(s.now()) {
		_ = s.store.DeleteSession(ctx, session.Token)
		return User{}, store.ErrNotFound
	}

	user, err := s.store.GetUserByID(ctx, session.UserID)
	if err != nil {
		return User{}, err
	}

	return userFromRecord(user), nil
}

func (s *Service) Logout(ctx context.Context, rawToken string) error {
	if strings.TrimSpace(rawToken) == "" {
		return nil
	}
	return s.store.DeleteSession(ctx, hashToken(rawToken))
}

func (s *Service) SessionCookieName() string {
	return sessionCookieName
}

func (s *Service) CSRFCookieName() string {
	return csrfCookieName
}

func (s *Service) SetSessionCookie(w http.ResponseWriter, r *http.Request, rawToken string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    rawToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.publicURLIsHTTPS(),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(s.sessionTTL.Seconds()),
	})
}

func (s *Service) ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.publicURLIsHTTPS(),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

func (s *Service) EnsureCSRFCookie(w http.ResponseWriter, r *http.Request) string {
	if cookie, err := r.Cookie(csrfCookieName); err == nil && strings.TrimSpace(cookie.Value) != "" {
		return cookie.Value
	}

	token := randomToken(24)
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: false,
		Secure:   s.publicURLIsHTTPS(),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int((24 * time.Hour).Seconds()),
	})
	return token
}

func (s *Service) ValidateCSRF(r *http.Request) bool {
	cookie, err := r.Cookie(csrfCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return false
	}

	formToken := strings.TrimSpace(r.FormValue("csrf_token"))
	return formToken != "" && cookie.Value == formToken
}

func ClientIP(r *http.Request) string {
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); forwarded != "" {
		parts := strings.Split(forwarded, ",")
		return strings.TrimSpace(parts[0])
	}

	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil {
		return host
	}

	return strings.TrimSpace(r.RemoteAddr)
}

func (s *Service) createSession(ctx context.Context, userID, ip, userAgent string) (string, error) {
	if err := s.store.DeleteExpiredSessions(ctx, s.now()); err != nil {
		return "", err
	}

	rawToken := randomToken(32)
	record := store.SessionRecord{
		Token:     hashToken(rawToken),
		UserID:    userID,
		ExpiresAt: s.now().Add(s.sessionTTL),
		IP:        ip,
		UserAgent: userAgent,
	}

	if err := s.store.CreateSession(ctx, record); err != nil {
		return "", err
	}

	return rawToken, nil
}

func (s *Service) allowIP(ip string) bool {
	if ip == "" {
		return true
	}

	s.ipMu.Lock()
	defer s.ipMu.Unlock()

	now := s.now()
	var kept []time.Time
	for _, attempt := range s.ipAttempts[ip] {
		if now.Sub(attempt) <= s.ipWindow {
			kept = append(kept, attempt)
		}
	}

	if len(kept) >= s.ipLimit {
		s.ipAttempts[ip] = kept
		return false
	}

	s.ipAttempts[ip] = append(kept, now)
	return true
}

func (s *Service) clearIPAttempts(ip string) {
	if ip == "" {
		return
	}

	s.ipMu.Lock()
	defer s.ipMu.Unlock()
	delete(s.ipAttempts, ip)
}

func (s *Service) validateTOTP(secret, code string, at time.Time) bool {
	valid, err := totp.ValidateCustom(code, secret, at, totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	return err == nil && valid
}

func (s *Service) encodeSecret(secret string) (string, error) {
	if s.secrets == nil {
		return secret, nil
	}

	encrypted, err := s.secrets.EncryptString(secret)
	if err != nil {
		return "", fmt.Errorf("encrypt totp secret: %w", err)
	}

	return "enc:" + encrypted, nil
}

func (s *Service) decodeSecret(secret string) (string, error) {
	if strings.HasPrefix(secret, "enc:") {
		if s.secrets == nil {
			return "", fmt.Errorf("encrypted secret present but master key is unavailable")
		}
		decrypted, err := s.secrets.DecryptString(strings.TrimPrefix(secret, "enc:"))
		if err != nil {
			return "", fmt.Errorf("decrypt totp secret: %w", err)
		}
		return decrypted, nil
	}

	return secret, nil
}

func (s *Service) publicURLIsHTTPS() bool {
	parsed, err := url.Parse(s.publicBaseURL)
	return err == nil && strings.EqualFold(parsed.Scheme, "https")
}

func userFromRecord(record store.UserRecord) User {
	return User{
		ID:        record.ID,
		Email:     record.Email,
		CreatedAt: record.CreatedAt,
	}
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func randomToken(size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("read random token: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}
