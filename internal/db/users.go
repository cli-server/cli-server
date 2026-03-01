package db

import (
	"database/sql"
	"fmt"
	"time"
)

type User struct {
	ID           string
	Username     string
	PasswordHash *string
	Email        *string
	Role         string
	CreatedAt    time.Time
}

func (db *DB) CreateUser(id, username, passwordHash string) error {
	_, err := db.Exec(
		"INSERT INTO users (id, username, password_hash) VALUES ($1, $2, $3)",
		id, username, passwordHash,
	)
	if err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

func (db *DB) CreateUserWithEmail(id, username string, passwordHash *string, email *string) error {
	_, err := db.Exec(
		"INSERT INTO users (id, username, password_hash, email) VALUES ($1, $2, $3, $4)",
		id, username, passwordHash, email,
	)
	if err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

func (db *DB) GetUserByUsername(username string) (*User, error) {
	u := &User{}
	err := db.QueryRow(
		"SELECT id, username, password_hash, email, role, created_at FROM users WHERE username = $1",
		username,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Email, &u.Role, &u.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user by username: %w", err)
	}
	return u, nil
}

func (db *DB) GetUserByID(id string) (*User, error) {
	u := &User{}
	err := db.QueryRow(
		"SELECT id, username, password_hash, email, role, created_at FROM users WHERE id = $1",
		id,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Email, &u.Role, &u.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user by id: %w", err)
	}
	return u, nil
}

func (db *DB) GetUserByEmail(email string) (*User, error) {
	u := &User{}
	err := db.QueryRow(
		"SELECT id, username, password_hash, email, role, created_at FROM users WHERE email = $1",
		email,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Email, &u.Role, &u.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user by email: %w", err)
	}
	return u, nil
}

func (db *DB) UpdateUserEmail(userID, email string) error {
	_, err := db.Exec("UPDATE users SET email = $1 WHERE id = $2", email, userID)
	if err != nil {
		return fmt.Errorf("update user email: %w", err)
	}
	return nil
}

func (db *DB) ListAllUsers() ([]*User, error) {
	rows, err := db.Query(
		"SELECT id, username, password_hash, email, role, created_at FROM users ORDER BY created_at ASC",
	)
	if err != nil {
		return nil, fmt.Errorf("list all users: %w", err)
	}
	defer rows.Close()

	var users []*User
	for rows.Next() {
		u := &User{}
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Email, &u.Role, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (db *DB) CountUsers() (int, error) {
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return count, nil
}

func (db *DB) UpdateUserRole(userID, role string) error {
	_, err := db.Exec("UPDATE users SET role = $1 WHERE id = $2", role, userID)
	if err != nil {
		return fmt.Errorf("update user role: %w", err)
	}
	return nil
}
