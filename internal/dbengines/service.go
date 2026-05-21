package dbengines

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"caddytower/internal/dockerx"
	"caddytower/internal/secrets"
	"caddytower/internal/store"

	mysql "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	enginePostgres = "pg"
	engineMariaDB  = "mariadb"

	postgresContainerName = "caddytower-postgres"
	mariadbContainerName  = "caddytower-mariadb"

	postgresVolumeName = "caddytower-postgres-data"
	mariadbVolumeName  = "caddytower-mariadb-data"

	settingPostgresRootPassword = "postgres_root_password"
	settingMariaDBRootPassword  = "mariadb_root_password"

	managedNetworkName = "edge"
)

var (
	identifierSanitizer = regexp.MustCompile(`[^a-z0-9_]+`)
	envVarNamePattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
)

type dockerService interface {
	InspectContainer(context.Context, string) (dockerx.ContainerInspect, error)
	RecreateContainer(context.Context, dockerx.ContainerSpec) (dockerx.ContainerInspect, error)
	Exec(context.Context, string, []string, []string, io.Writer, io.Writer) error
}

type Service struct {
	store   *store.Store
	secrets *secrets.Service
	docker  dockerService
}

type Attachment struct {
	ID         int64
	ProjectID  string
	Engine     string
	DBName     string
	DBUser     string
	Password   string
	EnvVarName string
	Host       string
	Port       int
}

func New(stateStore *store.Store, secretService *secrets.Service, dockerSvc dockerService) *Service {
	return &Service{
		store:   stateStore,
		secrets: secretService,
		docker:  dockerSvc,
	}
}

func (s *Service) AttachDatabase(ctx context.Context, projectID, slug, engine, envVarName string) (Attachment, error) {
	engine = strings.TrimSpace(engine)
	envVarName = strings.TrimSpace(envVarName)
	if envVarName == "" {
		envVarName = defaultEnvVar(engine)
	}
	if engine != enginePostgres && engine != engineMariaDB {
		return Attachment{}, fmt.Errorf("unsupported engine %q", engine)
	}
	if !envVarNamePattern.MatchString(envVarName) {
		return Attachment{}, fmt.Errorf("database connection env var must match [A-Za-z_][A-Za-z0-9_]*")
	}

	if _, err := s.ensureEngine(ctx, engine); err != nil {
		return Attachment{}, err
	}

	dbName := limitedIdentifier("ct_"+slug+"_"+strings.ToLower(envVarName), engine)
	dbUser := limitedIdentifier("u_"+slug+"_"+strings.ToLower(envVarName), engine)
	password := randomPassword(24)

	if err := s.provisionDatabase(ctx, engine, dbName, dbUser, password); err != nil {
		return Attachment{}, err
	}

	encodedPassword, err := s.encodeSecret(password)
	if err != nil {
		return Attachment{}, err
	}

	record, err := s.store.CreateDBAttachment(ctx, store.DBAttachmentRecord{
		ProjectID:  projectID,
		Engine:     engine,
		DBName:     dbName,
		DBUser:     dbUser,
		DBPassword: encodedPassword,
		EnvVarName: envVarName,
	})
	if err != nil {
		_ = s.dropDatabase(ctx, engine, dbName, dbUser)
		return Attachment{}, err
	}

	return Attachment{
		ID:         record.ID,
		ProjectID:  record.ProjectID,
		Engine:     record.Engine,
		DBName:     record.DBName,
		DBUser:     record.DBUser,
		Password:   password,
		EnvVarName: record.EnvVarName,
		Host:       engineHost(engine),
		Port:       enginePort(engine),
	}, nil
}

func (s *Service) ListAttachments(ctx context.Context, projectID string) ([]Attachment, error) {
	records, err := s.store.ListDBAttachmentsByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}

	attachments := make([]Attachment, 0, len(records))
	for _, record := range records {
		password, err := s.decodeSecret(record.DBPassword)
		if err != nil {
			return nil, err
		}
		attachments = append(attachments, Attachment{
			ID:         record.ID,
			ProjectID:  record.ProjectID,
			Engine:     record.Engine,
			DBName:     record.DBName,
			DBUser:     record.DBUser,
			Password:   password,
			EnvVarName: record.EnvVarName,
			Host:       engineHost(record.Engine),
			Port:       enginePort(record.Engine),
		})
	}

	return attachments, nil
}

func (s *Service) RotateAttachmentPassword(ctx context.Context, attachmentID int64) (Attachment, error) {
	record, err := s.store.GetDBAttachment(ctx, attachmentID)
	if err != nil {
		return Attachment{}, err
	}

	if _, err := s.ensureEngine(ctx, record.Engine); err != nil {
		return Attachment{}, err
	}

	password := randomPassword(24)
	if err := s.rotatePassword(ctx, record.Engine, record.DBUser, password); err != nil {
		return Attachment{}, err
	}

	encodedPassword, err := s.encodeSecret(password)
	if err != nil {
		return Attachment{}, err
	}
	if err := s.store.UpdateDBAttachmentPassword(ctx, attachmentID, encodedPassword); err != nil {
		return Attachment{}, err
	}

	return Attachment{
		ID:         record.ID,
		ProjectID:  record.ProjectID,
		Engine:     record.Engine,
		DBName:     record.DBName,
		DBUser:     record.DBUser,
		Password:   password,
		EnvVarName: record.EnvVarName,
		Host:       engineHost(record.Engine),
		Port:       enginePort(record.Engine),
	}, nil
}

func (s *Service) DeleteAttachment(ctx context.Context, attachmentID int64) error {
	record, err := s.store.GetDBAttachment(ctx, attachmentID)
	if err != nil {
		return err
	}

	if _, err := s.ensureEngine(ctx, record.Engine); err != nil {
		return err
	}

	if err := s.dropDatabase(ctx, record.Engine, record.DBName, record.DBUser); err != nil {
		return err
	}

	return s.store.DeleteDBAttachment(ctx, attachmentID)
}

func (s *Service) DumpAll(ctx context.Context, destDir string) ([]string, error) {
	if s.docker == nil {
		return nil, nil
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, fmt.Errorf("create engine backup dir: %w", err)
	}

	type engineDump struct {
		engine   string
		fileName string
		command  []string
		env      []string
	}

	dumps := []engineDump{
		{
			engine:   enginePostgres,
			fileName: "postgres.sql",
			command:  []string{"pg_dumpall", "-U", "postgres"},
		},
		{
			engine:   engineMariaDB,
			fileName: "mariadb.sql",
			command:  []string{"mariadb-dump", "-uroot", "--all-databases", "--single-transaction", "--quick", "--lock-tables=false"},
		},
	}

	var written []string
	for _, dump := range dumps {
		password, ok, err := s.rootPasswordIfPresent(ctx, dump.engine)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if _, err := s.ensureEngine(ctx, dump.engine); err != nil {
			return nil, err
		}

		env := append([]string(nil), dump.env...)
		if dump.engine == enginePostgres {
			env = append(env, "PGPASSWORD="+password)
		} else {
			env = append(env, "MYSQL_PWD="+password)
		}

		path := filepath.Join(destDir, dump.fileName)
		file, err := os.Create(path)
		if err != nil {
			return nil, fmt.Errorf("create %s backup file: %w", dump.engine, err)
		}

		var stderr bytes.Buffer
		err = s.docker.Exec(ctx, engineHost(dump.engine), dump.command, env, file, &stderr)
		closeErr := file.Close()
		if err != nil {
			_ = os.Remove(path)
			if stderr.Len() > 0 {
				return nil, fmt.Errorf("%s backup failed: %w: %s", dump.engine, err, strings.TrimSpace(stderr.String()))
			}
			return nil, fmt.Errorf("%s backup failed: %w", dump.engine, err)
		}
		if closeErr != nil {
			_ = os.Remove(path)
			return nil, fmt.Errorf("close %s backup file: %w", dump.engine, closeErr)
		}

		written = append(written, path)
	}

	return written, nil
}

func (s *Service) ensureEngine(ctx context.Context, engine string) (dockerx.ContainerInspect, error) {
	name := engineHost(engine)
	if s.docker == nil {
		return dockerx.ContainerInspect{}, fmt.Errorf("docker service is unavailable")
	}

	if inspect, err := s.docker.InspectContainer(ctx, name); err == nil && inspect.Running {
		return inspect, nil
	}

	password, err := s.rootPassword(ctx, engine)
	if err != nil {
		return dockerx.ContainerInspect{}, err
	}

	spec := dockerx.ContainerSpec{
		Name:          name,
		Image:         engineImage(engine),
		Network:       managedNetworkName,
		RestartPolicy: "unless-stopped",
		Mounts: []dockerx.Mount{{
			Source: engineVolume(engine),
			Target: engineDataDir(engine),
		}},
		ExposedPorts: []string{fmt.Sprintf("%d", enginePort(engine))},
		Labels: map[string]string{
			"caddytower.managed": "true",
			"caddytower.engine":  engine,
		},
		Env: engineEnv(engine, password),
	}

	if _, err := s.docker.RecreateContainer(ctx, spec); err != nil {
		return dockerx.ContainerInspect{}, err
	}

	if err := s.waitForReady(ctx, engine, password); err != nil {
		return dockerx.ContainerInspect{}, err
	}

	return s.docker.InspectContainer(ctx, name)
}

func (s *Service) rootPassword(ctx context.Context, engine string) (string, error) {
	password, ok, err := s.rootPasswordIfPresent(ctx, engine)
	if err != nil {
		return "", err
	}
	if ok {
		return password, nil
	}

	key := rootPasswordSetting(engine)
	password = randomPassword(30)
	encoded, err := s.encodeSecret(password)
	if err != nil {
		return "", err
	}

	if err := s.store.UpsertSettings(ctx, map[string]string{key: encoded}); err != nil {
		return "", err
	}
	return password, nil
}

func (s *Service) rootPasswordIfPresent(ctx context.Context, engine string) (string, bool, error) {
	key := rootPasswordSetting(engine)
	values, err := s.store.GetSettings(ctx, key)
	if err != nil {
		return "", false, err
	}

	existing := strings.TrimSpace(values[key])
	if existing == "" {
		return "", false, nil
	}

	password, err := s.decodeSecret(existing)
	return password, true, err
}

func rootPasswordSetting(engine string) string {
	key := settingPostgresRootPassword
	if engine == engineMariaDB {
		key = settingMariaDBRootPassword
	}
	return key
}

func (s *Service) provisionDatabase(ctx context.Context, engine, dbName, dbUser, password string) error {
	switch engine {
	case enginePostgres:
		db, err := s.openPostgresAdmin(ctx)
		if err != nil {
			return err
		}
		defer db.Close()

		if _, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE USER "%s" WITH PASSWORD '%s'`, dbUser, escapeSQLLiteral(password))); err != nil {
			return fmt.Errorf("create postgres user: %w", err)
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE DATABASE "%s" OWNER "%s"`, dbName, dbUser)); err != nil {
			_, _ = db.ExecContext(ctx, fmt.Sprintf(`DROP USER IF EXISTS "%s"`, dbUser))
			return fmt.Errorf("create postgres database: %w", err)
		}
	case engineMariaDB:
		db, err := s.openMariaDBAdmin(ctx)
		if err != nil {
			return err
		}
		defer db.Close()

		if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE `%s`", dbName)); err != nil {
			return fmt.Errorf("create mariadb database: %w", err)
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE USER '%s'@'%%' IDENTIFIED BY '%s'`, dbUser, escapeSQLLiteral(password))); err != nil {
			_, _ = db.ExecContext(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", dbName))
			return fmt.Errorf("create mariadb user: %w", err)
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf("GRANT ALL PRIVILEGES ON `%s`.* TO '%s'@'%%'", dbName, dbUser)); err != nil {
			return fmt.Errorf("grant mariadb privileges: %w", err)
		}
		if _, err := db.ExecContext(ctx, `FLUSH PRIVILEGES`); err != nil {
			return fmt.Errorf("flush mariadb privileges: %w", err)
		}
	default:
		return fmt.Errorf("unsupported engine %q", engine)
	}

	return nil
}

func (s *Service) rotatePassword(ctx context.Context, engine, dbUser, password string) error {
	switch engine {
	case enginePostgres:
		db, err := s.openPostgresAdmin(ctx)
		if err != nil {
			return err
		}
		defer db.Close()

		if _, err := db.ExecContext(ctx, fmt.Sprintf(`ALTER USER "%s" WITH PASSWORD '%s'`, dbUser, escapeSQLLiteral(password))); err != nil {
			return fmt.Errorf("rotate postgres password: %w", err)
		}
	case engineMariaDB:
		db, err := s.openMariaDBAdmin(ctx)
		if err != nil {
			return err
		}
		defer db.Close()

		if _, err := db.ExecContext(ctx, fmt.Sprintf(`ALTER USER '%s'@'%%' IDENTIFIED BY '%s'`, dbUser, escapeSQLLiteral(password))); err != nil {
			return fmt.Errorf("rotate mariadb password: %w", err)
		}
		if _, err := db.ExecContext(ctx, `FLUSH PRIVILEGES`); err != nil {
			return fmt.Errorf("flush mariadb privileges: %w", err)
		}
	default:
		return fmt.Errorf("unsupported engine %q", engine)
	}

	return nil
}

func (s *Service) dropDatabase(ctx context.Context, engine, dbName, dbUser string) error {
	switch engine {
	case enginePostgres:
		db, err := s.openPostgresAdmin(ctx)
		if err != nil {
			return err
		}
		defer db.Close()

		if _, err := db.ExecContext(ctx, fmt.Sprintf(`
			SELECT pg_terminate_backend(pid)
			FROM pg_stat_activity
			WHERE datname = '%s' AND pid <> pg_backend_pid()
		`, escapeSQLLiteral(dbName))); err != nil {
			return fmt.Errorf("terminate postgres sessions: %w", err)
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS "%s"`, dbName)); err != nil {
			return fmt.Errorf("drop postgres database: %w", err)
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`DROP USER IF EXISTS "%s"`, dbUser)); err != nil {
			return fmt.Errorf("drop postgres user: %w", err)
		}
	case engineMariaDB:
		db, err := s.openMariaDBAdmin(ctx)
		if err != nil {
			return err
		}
		defer db.Close()

		if _, err := db.ExecContext(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", dbName)); err != nil {
			return fmt.Errorf("drop mariadb database: %w", err)
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`DROP USER IF EXISTS '%s'@'%%'`, dbUser)); err != nil {
			return fmt.Errorf("drop mariadb user: %w", err)
		}
		if _, err := db.ExecContext(ctx, `FLUSH PRIVILEGES`); err != nil {
			return fmt.Errorf("flush mariadb privileges: %w", err)
		}
	default:
		return fmt.Errorf("unsupported engine %q", engine)
	}

	return nil
}

func (s *Service) waitForReady(ctx context.Context, engine, password string) error {
	deadline := time.Now().Add(45 * time.Second)
	for {
		var err error
		switch engine {
		case enginePostgres:
			var db *sql.DB
			db, err = sql.Open("pgx", fmt.Sprintf("postgres://postgres:%s@%s:%d/postgres?sslmode=disable", urlQueryEscape(password), engineHost(engine), enginePort(engine)))
			if err == nil {
				err = db.PingContext(ctx)
				_ = db.Close()
			}
		case engineMariaDB:
			var db *sql.DB
			db, err = sql.Open("mysql", mariaDBDSN("root", password, engineHost(engine), enginePort(engine), "mysql"))
			if err == nil {
				err = db.PingContext(ctx)
				_ = db.Close()
			}
		}
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("wait for %s readiness: %w", engine, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
}

func (s *Service) openPostgresAdmin(ctx context.Context) (*sql.DB, error) {
	password, err := s.rootPassword(ctx, enginePostgres)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("pgx", fmt.Sprintf("postgres://postgres:%s@%s:%d/postgres?sslmode=disable", urlQueryEscape(password), engineHost(enginePostgres), enginePort(enginePostgres)))
	if err != nil {
		return nil, fmt.Errorf("open postgres admin db: %w", err)
	}
	return db, db.PingContext(ctx)
}

func (s *Service) openMariaDBAdmin(ctx context.Context) (*sql.DB, error) {
	password, err := s.rootPassword(ctx, engineMariaDB)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("mysql", mariaDBDSN("root", password, engineHost(engineMariaDB), enginePort(engineMariaDB), "mysql"))
	if err != nil {
		return nil, fmt.Errorf("open mariadb admin db: %w", err)
	}
	return db, db.PingContext(ctx)
}

func (s *Service) encodeSecret(value string) (string, error) {
	if s.secrets == nil || value == "" {
		return value, nil
	}
	encrypted, err := s.secrets.EncryptString(value)
	if err != nil {
		return "", err
	}
	return "enc:" + encrypted, nil
}

func (s *Service) decodeSecret(value string) (string, error) {
	if !strings.HasPrefix(value, "enc:") {
		return value, nil
	}
	if s.secrets == nil {
		return "", fmt.Errorf("encrypted secret present but master key is unavailable")
	}
	return s.secrets.DecryptString(strings.TrimPrefix(value, "enc:"))
}

func engineImage(engine string) string {
	if engine == engineMariaDB {
		return "mariadb:11"
	}
	return "postgres:16-alpine"
}

func engineVolume(engine string) string {
	if engine == engineMariaDB {
		return mariadbVolumeName
	}
	return postgresVolumeName
}

func engineDataDir(engine string) string {
	if engine == engineMariaDB {
		return "/var/lib/mysql"
	}
	return "/var/lib/postgresql/data"
}

func engineHost(engine string) string {
	if engine == engineMariaDB {
		return mariadbContainerName
	}
	return postgresContainerName
}

func enginePort(engine string) int {
	if engine == engineMariaDB {
		return 3306
	}
	return 5432
}

func engineEnv(engine, password string) map[string]string {
	if engine == engineMariaDB {
		return map[string]string{
			"MARIADB_ROOT_PASSWORD": password,
		}
	}
	return map[string]string{
		"POSTGRES_PASSWORD": password,
	}
}

func defaultEnvVar(engine string) string {
	if engine == engineMariaDB {
		return "MYSQL_URL"
	}
	return "DATABASE_URL"
}

func limitedIdentifier(raw, engine string) string {
	value := strings.ToLower(raw)
	value = strings.ReplaceAll(value, "-", "_")
	value = identifierSanitizer.ReplaceAllString(value, "_")
	value = strings.Trim(value, "_")
	if value == "" {
		value = "ct_app"
	}
	limit := 63
	if engine == engineMariaDB {
		limit = 80
	}
	if len(value) > limit {
		value = value[:limit]
	}
	return value
}

func randomPassword(length int) string {
	// Keep generated passwords DSN-safe because attachments are surfaced as raw
	// connection strings to applications that may not decode escaped credentials.
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789-_"
	var out strings.Builder
	for i := 0; i < length; i++ {
		out.WriteByte(alphabet[rand.IntN(len(alphabet))])
	}
	return out.String()
}

func mariaDBDSN(user, password, host string, port int, database string) string {
	cfg := mysql.Config{
		User:   user,
		Passwd: password,
		Net:    "tcp",
		Addr:   fmt.Sprintf("%s:%d", host, port),
		DBName: database,
		Params: map[string]string{
			"parseTime": "true",
		},
	}
	return cfg.FormatDSN()
}

func escapeSQLLiteral(value string) string {
	return strings.ReplaceAll(value, `'`, `''`)
}

func urlQueryEscape(value string) string {
	replacer := strings.NewReplacer(
		"%", "%25",
		"/", "%2F",
		":", "%3A",
		"@", "%40",
		"?", "%3F",
		"&", "%26",
		"=", "%3D",
		"+", "%2B",
		"#", "%23",
	)
	return replacer.Replace(value)
}
