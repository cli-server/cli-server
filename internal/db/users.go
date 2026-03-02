package db

import (
	"database/sql"
	"fmt"
	"time"
)

type User struct {
	ID        string
	Username  string
	Email     string
	Name      *string
	Picture   *string
	Role      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (db *DB) CreateUser(id, username, email, passwordHash string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		"INSERT INTO users (id, username, email) VALUES ($1, $2, $3)",
		id, username, email,
	)
	if err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	_, err = tx.Exec(
		"INSERT INTO user_credentials (user_id, password_hash) VALUES ($1, $2)",
		id, passwordHash,
	)
	if err != nil {
		return fmt.Errorf("create user credentials: %w", err)
	}
	return tx.Commit()
}

func (db *DB) CreateUserWithEmail(id, username string, passwordHash *string, email string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		"INSERT INTO users (id, username, email) VALUES ($1, $2, $3)",
		id, username, email,
	)
	if err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	if passwordHash != nil {
		_, err = tx.Exec(
			"INSERT INTO user_credentials (user_id, password_hash) VALUES ($1, $2)",
			id, *passwordHash,
		)
		if err != nil {
			return fmt.Errorf("create user credentials: %w", err)
		}
	}
	return tx.Commit()
}

func (db *DB) GetUserByUsername(username string) (*User, error) {
	u := &User{}
	err := db.QueryRow(
		"SELECT id, username, email, name, picture, role, created_at, updated_at FROM users WHERE username = $1",
		username,
	).Scan(&u.ID, &u.Username, &u.Email, &u.Name, &u.Picture, &u.Role, &u.CreatedAt, &u.UpdatedAt)
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
		"SELECT id, username, email, name, picture, role, created_at, updated_at FROM users WHERE id = $1",
		id,
	).Scan(&u.ID, &u.Username, &u.Email, &u.Name, &u.Picture, &u.Role, &u.CreatedAt, &u.UpdatedAt)
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
		"SELECT id, username, email, name, picture, role, created_at, updated_at FROM users WHERE email = $1",
		email,
	).Scan(&u.ID, &u.Username, &u.Email, &u.Name, &u.Picture, &u.Role, &u.CreatedAt, &u.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user by email: %w", err)
	}
	return u, nil
}

func (db *DB) UpdateUserEmail(userID, email string) error {
	_, err := db.Exec("UPDATE users SET email = $1, updated_at = NOW() WHERE id = $2", email, userID)
	if err != nil {
		return fmt.Errorf("update user email: %w", err)
	}
	return nil
}

func (db *DB) ListAllUsers() ([]*User, error) {
	rows, err := db.Query(
		"SELECT id, username, email, name, picture, role, created_at, updated_at FROM users ORDER BY created_at ASC",
	)
	if err != nil {
		return nil, fmt.Errorf("list all users: %w", err)
	}
	defer rows.Close()

	var users []*User
	for rows.Next() {
		u := &User{}
		if err := rows.Scan(&u.ID, &u.Username, &u.Email, &u.Name, &u.Picture, &u.Role, &u.CreatedAt, &u.UpdatedAt); err != nil {
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
	_, err := db.Exec("UPDATE users SET role = $1, updated_at = NOW() WHERE id = $2", role, userID)
	if err != nil {
		return fmt.Errorf("update user role: %w", err)
	}
	return nil
}

func (db *DB) UpdateUserPicture(userID, picture string) error {
	_, err := db.Exec("UPDATE users SET picture = $1, updated_at = NOW() WHERE id = $2", picture, userID)
	if err != nil {
		return fmt.Errorf("update user picture: %w", err)
	}
	return nil
}

func (db *DB) UpdateUserName(userID, name string) error {
	_, err := db.Exec("UPDATE users SET name = $1, updated_at = NOW() WHERE id = $2", name, userID)
	if err != nil {
		return fmt.Errorf("update user name: %w", err)
	}
	return nil
}
