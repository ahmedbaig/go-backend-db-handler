package appconfig

import (
	"fmt"
	"net"
	"net/url"
	"strconv"

	"github.com/ahmedbaig/go-backend-config"
)

type App struct {
	Port           int             `json:"port" validate:"min=0,max=65535"`
	Host           string          `json:"host"`
	Environment    string          `json:"environment" validate:"oneof=localhost testing staging production"`
	RequestTimeout config.Duration `json:"requestTimeout"`
	DefaultTake    int             `json:"defaultTake" validate:"min=0"`
	MaxTake        int             `json:"maxTake" validate:"min=0"`
	APIKeys        []string        `json:"apiKeys"`
}

func DefaultApp() App {
	return App{Port: 8090, Host: "0.0.0.0", Environment: "production", RequestTimeout: config.Duration(30e9), DefaultTake: 100, MaxTake: 1000}
}

func (a App) Addr() string { return net.JoinHostPort(a.Host, strconv.Itoa(a.Port)) }

type Connection struct {
	Database        string            `json:"database" validate:"required"`
	Instance        string            `json:"instance"`
	Driver          string            `json:"driver" validate:"required,oneof=postgres mysql sqlserver sqlite"`
	DSN             string            `json:"dsn"`
	Host            string            `json:"host"`
	Port            int               `json:"port"`
	User            string            `json:"user"`
	Password        string            `json:"password"`
	Name            string            `json:"name"`
	Params          map[string]string `json:"params"`
	MaxOpenConns    int               `json:"maxOpenConns"`
	MaxIdleConns    int               `json:"maxIdleConns"`
	ConnMaxLifetime config.Duration   `json:"connMaxLifetime"`
	ConnMaxIdleTime config.Duration   `json:"connMaxIdleTime"`
	SlowQuery       config.Duration   `json:"slowQuery"`
	Default         bool              `json:"default"`
}

type DB struct {
	Connections []Connection `json:"connections" validate:"dive"`
}

func (d DB) Validate() error {
	seen := map[string]bool{}
	for _, c := range d.Connections {
		key := c.Database + "@" + c.Instance
		if seen[key] {
			return fmt.Errorf("connection %s is defined twice", key)
		}
		seen[key] = true
		if c.DSN == "" && c.Driver != "sqlite" && c.Host == "" {
			return fmt.Errorf("connection %s needs dsn or host", key)
		}
		if c.Driver == "sqlite" && c.DSN == "" && c.Name == "" {
			return fmt.Errorf("connection %s needs dsn or name (file path)", key)
		}
	}
	return nil
}

func (c Connection) Key() string {
	if c.Instance == "" {
		return c.Database
	}
	return c.Database + "@" + c.Instance
}

func (c Connection) BuildDSN() string {
	if c.DSN != "" {
		return c.DSN
	}
	switch c.Driver {
	case "postgres":
		u := url.URL{Scheme: "postgres", User: url.UserPassword(c.User, c.Password), Host: hostPort(c.Host, c.Port, 5432), Path: "/" + c.Name}
		q := url.Values{}
		for k, v := range c.Params {
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
		return u.String()
	case "mysql":
		q := url.Values{"parseTime": {"true"}, "charset": {"utf8mb4"}, "loc": {"UTC"}}
		for k, v := range c.Params {
			q.Set(k, v)
		}
		return fmt.Sprintf("%s:%s@tcp(%s)/%s?%s", c.User, c.Password, hostPort(c.Host, c.Port, 3306), c.Name, q.Encode())
	case "sqlserver":
		u := url.URL{Scheme: "sqlserver", User: url.UserPassword(c.User, c.Password), Host: hostPort(c.Host, c.Port, 1433)}
		q := url.Values{"database": {c.Name}}
		for k, v := range c.Params {
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
		return u.String()
	case "sqlite":
		return c.Name
	}
	return ""
}

func hostPort(h string, p, def int) string {
	if p == 0 {
		p = def
	}
	return net.JoinHostPort(h, strconv.Itoa(p))
}
