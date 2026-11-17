package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	driver "github.com/go-sql-driver/mysql"
	identity "github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/authentication"
)

type MySQLUsers struct{ db *sql.DB }

func NewMySQLUsers(db *sql.DB) *MySQLUsers { return &MySQLUsers{db: db} }
func (r *MySQLUsers) CreateUser(ctx context.Context, user identity.User) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,status) VALUES(?,?,?,?)`, user.ID, user.Username, user.PasswordHash, user.Status)
	if duplicate(err) {
		return identity.ErrUsernameExists
	}
	if err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}
func (r *MySQLUsers) GetUserByID(ctx context.Context, id string) (identity.User, error) {
	return r.get(ctx, `SELECT id,username,password_hash,status,created_at,updated_at FROM users WHERE id=?`, id)
}
func (r *MySQLUsers) GetUserByUsername(ctx context.Context, name string) (identity.User, error) {
	return r.get(ctx, `SELECT id,username,password_hash,status,created_at,updated_at FROM users WHERE username=?`, name)
}
func (r *MySQLUsers) get(ctx context.Context, query, value string) (identity.User, error) {
	var user identity.User
	err := r.db.QueryRowContext(ctx, query, value).Scan(&user.ID, &user.Username, &user.PasswordHash, &user.Status, &user.CreatedAt, &user.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.User{}, identity.ErrUserNotFound
	}
	if err != nil {
		return identity.User{}, fmt.Errorf("get user: %w", err)
	}
	return user, nil
}
func (r *MySQLUsers) UpdatePassword(ctx context.Context, id, hash string) error {
	return r.update(ctx, `UPDATE users SET password_hash=? WHERE id=?`, hash, id)
}
func (r *MySQLUsers) SetUserStatus(ctx context.Context, id string, status identity.UserStatus) error {
	return r.update(ctx, `UPDATE users SET status=? WHERE id=?`, status, id)
}
func (r *MySQLUsers) update(ctx context.Context, query string, args ...any) error {
	result, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return identity.ErrUserNotFound
	}
	return nil
}
func duplicate(err error) bool {
	var mysqlErr *driver.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062
}

var _ identity.UserRepository = (*MySQLUsers)(nil)
