package model

import (
	"context"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"

	"git.sr.ht/~sircmpwn/git.sr.ht/api/database"
)

type Repository struct {
	ID             int        `json:"id"`
	Created        time.Time  `json:"created"`
	Updated        time.Time  `json:"updated"`
	Name           string     `json:"name"`
	Description    *string    `json:"description"`
	Visibility     Visibility `json:"visibility"`
	UpstreamURL    *string    `json:"upstreamUrl"`
	Objects        []Object   `json:"objects"`
	Log            []*Commit  `json:"log"`
	Tree           *Tree      `json:"tree"`
	File           *Blob      `json:"file"`
	RevparseSingle Object     `json:"revparse_single"`

	Path    string
	OwnerID int

	alias string
	repo  *git.Repository
}

func (r *Repository) Repo() *git.Repository {
	if r.repo != nil {
		return r.repo
	}
	var err error
	r.repo, err = git.PlainOpen(r.Path)
	if err != nil {
		panic(err)
	}
	return r.repo
}

func (r *Repository) Head() *Reference {
	ref, err := r.Repo().Head()
	if err != nil {
		panic(err)
	}
	return &Reference{Ref: ref, Repo: r.repo}
}

func (r *Repository) Columns(ctx context.Context, tbl string) string {
	columns := ColumnsFor(ctx, map[string]string{
		"id":          "id",
		"created":     "created",
		"updated":     "updated",
		"name":        "name",
		"description": "description",
		"visibility":  "visibility",
		"upstreamUrl": "upstream_uri",
	}, tbl)
	return strings.Join(append(columns, tbl+".path", tbl+".owner_id"), ", ")
}

func (r *Repository) Select(ctx context.Context) []string {
	return append(database.ColumnsFor(ctx, r.alias, map[string]string{
		"id":          "id",
		"created":     "created",
		"updated":     "updated",
		"name":        "name",
		"description": "description",
		"visibility":  "visibility",
		"upstreamUrl": "upstream_uri",
	}),
		database.WithAlias(r.alias, "path"),
		database.WithAlias(r.alias, "owner_id"))
}

func (r *Repository) As(alias string) database.Selectable {
	r.alias = alias
	return r
}

func (r *Repository) Fields(ctx context.Context) []interface{} {
	fields := FieldsFor(ctx, map[string]interface{}{
		"id":           &r.ID,
		"created":      &r.Created,
		"updated":      &r.Updated,
		"name":         &r.Name,
		"description":  &r.Description,
		"visibility":   &r.Visibility,
		"upstream_url": &r.UpstreamURL,
	})
	return append(fields, &r.Path, &r.OwnerID)
}
