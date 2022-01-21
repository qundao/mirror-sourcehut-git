package graph

// This file will be automatically regenerated based on the schema, any resolver implementations
// will be copied through when generating and any unknown code will be moved to the end.

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	"git.sr.ht/~sircmpwn/core-go/auth"
	"git.sr.ht/~sircmpwn/core-go/config"
	"git.sr.ht/~sircmpwn/core-go/database"
	coremodel "git.sr.ht/~sircmpwn/core-go/model"
	"git.sr.ht/~sircmpwn/core-go/server"
	corewebhooks "git.sr.ht/~sircmpwn/core-go/webhooks"
	"git.sr.ht/~sircmpwn/git.sr.ht/api/graph/api"
	"git.sr.ht/~sircmpwn/git.sr.ht/api/graph/model"
	"git.sr.ht/~sircmpwn/git.sr.ht/api/loaders"
	"git.sr.ht/~sircmpwn/git.sr.ht/api/webhooks"
	"github.com/99designs/gqlgen/graphql"
	sq "github.com/Masterminds/squirrel"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/lib/pq"
	minio "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func (r *aCLResolver) Repository(ctx context.Context, obj *model.ACL) (*model.Repository, error) {
	return loaders.ForContext(ctx).RepositoriesByID.Load(obj.RepoID)
}

func (r *aCLResolver) Entity(ctx context.Context, obj *model.ACL) (model.Entity, error) {
	return loaders.ForContext(ctx).UsersByID.Load(obj.UserID)
}

func (r *artifactResolver) URL(ctx context.Context, obj *model.Artifact) (string, error) {
	conf := config.ForContext(ctx)
	upstream, ok := conf.Get("objects", "s3-upstream")
	if !ok {
		return "", fmt.Errorf("S3 upstream not configured for this server")
	}
	bucket, ok := conf.Get("git.sr.ht", "s3-bucket")
	if !ok {
		return "", fmt.Errorf("S3 bucket not configured for this server")
	}
	prefix, ok := conf.Get("git.sr.ht", "s3-prefix")
	if !ok {
		return "", fmt.Errorf("S3 prefix not configured for this server")
	}
	return fmt.Sprintf("https://%s/%s/%s/%s", upstream, bucket, prefix, obj.Filename), nil
}

func (r *commitResolver) Diff(ctx context.Context, obj *model.Commit) (string, error) {
	return obj.DiffContext(ctx), nil
}

func (r *mutationResolver) CreateRepository(ctx context.Context, name string, visibility model.Visibility, description *string, cloneURL *string) (*model.Repository, error) {
	if !repoNameRE.MatchString(name) {
		return nil, fmt.Errorf("Invalid repository name '%s' (must match %s)",
			name, repoNameRE.String())
	}
	if name == "." || name == ".." {
		return nil, fmt.Errorf("Invalid repository name '%s' (must not be . or ..)", name)
	}
	if name == ".git" || name == ".hg" {
		return nil, fmt.Errorf("Invalid repository name '%s' (must not be .git or .hg)", name)
	}

	conf := config.ForContext(ctx)
	repoStore, ok := conf.Get("git.sr.ht", "repos")
	if !ok || repoStore == "" {
		panic(fmt.Errorf("Configuration error: [git.sr.ht]repos is unset"))
	}
	postUpdate, ok := conf.Get("git.sr.ht", "post-update-script")
	if !ok {
		panic(fmt.Errorf("Configuration error: [git.sr.ht]post-update is unset"))
	}

	user := auth.ForContext(ctx)
	repoPath := path.Join(repoStore, "~"+user.Username, name)

	var (
		repoCreated bool
		repo        model.Repository
	)
	defer func() {
		if err := recover(); err != nil {
			if repoCreated {
				err := os.RemoveAll(repoPath)
				if err != nil {
					panic(err)
				}
			}
			panic(err)
		}
	}()

	if err := database.WithTx(ctx, nil, func(tx *sql.Tx) error {
		vismap := map[model.Visibility]string{
			model.VisibilityPublic:   "public",
			model.VisibilityUnlisted: "unlisted",
			model.VisibilityPrivate:  "private",
		}
		var (
			dvis string
			ok   bool
		)
		if dvis, ok = vismap[visibility]; !ok {
			panic(fmt.Errorf("Unknown visibility %s", visibility)) // Invariant
		}

		row := tx.QueryRowContext(ctx, `
			INSERT INTO repository (
				created, updated, name, description, path, visibility, owner_id
			) VALUES (
				NOW() at time zone 'utc',
				NOW() at time zone 'utc',
				$1, $2, $3, $4, $5
			) RETURNING 
				id, created, updated, name, description, visibility,
				upstream_uri, path, owner_id;
		`, name, description, repoPath, dvis, user.UserID)
		if err := row.Scan(&repo.ID, &repo.Created, &repo.Updated, &repo.Name,
			&repo.Description, &repo.RawVisibility, &repo.UpstreamURL,
			&repo.Path, &repo.OwnerID); err != nil {
			if strings.Contains(err.Error(), "duplicate key value violates unique constraint") {
				return gqlErrorf(ctx, "name", "A repository with this name already exists.")
			}
			return err
		}

		var gitrepo *git.Repository
		if cloneURL != nil {
			u, err := url.Parse(*cloneURL)
			if err != nil {
				return err
			} else if u.Host == "" {
				return gqlErrorf(ctx, "cloneUrl", "Cannot use URL without host")
			} else if _, ok := allowedCloneSchemes[u.Scheme]; !ok {
				return gqlErrorf(ctx, "cloneUrl", "Unsupported protocol %q", u.Scheme)
			}

			origin := config.GetOrigin(conf, "git.sr.ht", true)
			o, err := url.Parse(origin)
			if err != nil {
				panic(err)
			}

			// Check if this is a local repository
			if u.Scheme == o.Scheme && u.Host == o.Host {
				u.Path = strings.TrimPrefix(u.Path, "/")
				split := strings.SplitN(u.Path, "/", 2)
				if len(split) != 2 {
					return gqlErrorf(ctx, "cloneUrl", "Invalid clone URL")
				}
				canonicalName, repoName := split[0], split[1]
				entity := canonicalName
				if strings.HasPrefix(entity, "~") {
					entity = entity[1:]
				} else {
					return gqlErrorf(ctx, "cloneUrl", "Invalid username")
				}
				repo, err := loaders.ForContext(ctx).
					RepositoriesByOwnerRepoName.Load([2]string{entity, repoName})
				if err != nil {
					return err
				} else if repo == nil {
					return gqlErrorf(ctx, "cloneUrl", "Repository not found")
				}
				cloneURL = &repo.Path
			}

			gitrepo, err = git.PlainClone(repoPath, true, &git.CloneOptions{
				URL:               *cloneURL,
				RecurseSubmodules: git.NoRecurseSubmodules,
			})
			if err != nil {
				return gqlErrorf(ctx, "cloneUrl", "%s", err)
			}
		} else {
			var err error
			gitrepo, err = git.PlainInit(repoPath, true)
			if err != nil {
				return err
			}
		}

		repoCreated = true
		config, err := gitrepo.Config()
		if err != nil {
			return err
		}
		config.Raw.SetOption("core", "", "repositoryformatversion", "0")
		config.Raw.SetOption("core", "", "filemode", "true")
		config.Raw.SetOption("srht", "", "repo-id", strconv.Itoa(repo.ID))
		config.Raw.SetOption("receive", "", "denyDeleteCurrent", "ignore")
		config.Raw.SetOption("receive", "", "advertisePushOptions", "true")
		if err = gitrepo.Storer.SetConfig(config); err != nil {
			return err
		}

		hookdir := path.Join(repoPath, "hooks")
		if err = os.Mkdir(hookdir, os.ModePerm); err != nil {
			return err
		}
		for _, hook := range []string{"pre-receive", "update", "post-update"} {
			if err = os.Symlink(postUpdate, path.Join(hookdir, hook)); err != nil {
				return err
			}
		}

		webhooks.DeliverRepoEvent(ctx, model.WebhookEventRepoCreated, &repo)
		webhooks.DeliverLegacyRepoCreate(ctx, &repo)
		return nil
	}); err != nil {
		if repoCreated {
			err := os.RemoveAll(repoPath)
			if err != nil {
				panic(err)
			}
		}
		return nil, err
	}

	return &repo, nil
}

func (r *mutationResolver) UpdateRepository(ctx context.Context, id int, input map[string]interface{}) (*model.Repository, error) {
	// This function is somewhat involved, because it has to address repository
	// renames. The process is the following:
	//
	// 1. Prepare, but don't execute, a normal update operation
	// 2. If a rename is requested, insert a redirect and move the repo on disk.
	// 3. Complete the normal update operation.
	//
	// We need 2 to happen between 1 and 3 because step 3 is going to change
	// information in the database that step 2 relies on, and step 3 has to
	// happen after step 2 because step 2 will change something (the path) that
	// step 3 relies on.
	//
	// Additional complication: if an error occurs after the directory has been
	// moved on disk, we have to move it back.
	var (
		repo     model.Repository
		origPath string
		repoPath string
		moved    bool
	)

	defer func() {
		if err := recover(); err != nil {
			if moved {
				err := os.Rename(repoPath, origPath)
				if err != nil {
					panic(err)
				}
			}
			panic(err)
		}
	}()

	if err := database.WithTx(ctx, nil, func(tx *sql.Tx) error {
		user := auth.ForContext(ctx)

		if val, ok := input["visibility"]; ok {
			delete(input, "visibility")
			vis := model.Visibility(val.(string))
			if !vis.IsValid() {
				return fmt.Errorf("Invalid visibility '%s'", val)
			}
			switch vis {
			case model.VisibilityPublic:
				input["visibility"] = "public"
			case model.VisibilityUnlisted:
				input["visibility"] = "unlisted"
			case model.VisibilityPrivate:
				input["visibility"] = "private"
			default:
				panic("Invariant broken; unknown visibility")
			}
		}

		query := database.Apply(&repo, input).
			Where(`id = ?`, id).
			Where(`owner_id = ?`, auth.ForContext(ctx).UserID).
			Set(`updated`, sq.Expr(`now() at time zone 'utc'`)).
			Suffix(`RETURNING
				id, created, updated, name, description, visibility,
				upstream_uri, path, owner_id`)

		if n, ok := input["name"]; ok {
			name, ok := n.(string)
			if !ok {
				return fmt.Errorf("Invalid type for 'name' field (expected string)")
			}

			if !repoNameRE.MatchString(name) {
				return fmt.Errorf("Invalid repository name '%s' (must match %s)",
					name, repoNameRE.String())
			}
			if name == "." || name == ".." {
				return fmt.Errorf("Invalid repository name '%s' (must not be . or ..)", name)
			}
			if name == ".git" || name == ".hg" {
				return fmt.Errorf("Invalid repository name '%s' (must not be .git or .hg)", name)
			}

			var origPath string
			row := tx.QueryRowContext(ctx, `
				INSERT INTO redirect (
					created, name, path, owner_id, new_repo_id
				) SELECT 
					NOW() at time zone 'utc',
					orig.name, orig.path, orig.owner_id, orig.id
				FROM repository orig
				WHERE id = $1 AND owner_id = $2
				RETURNING path;
			`, id, auth.ForContext(ctx).UserID)
			if err := row.Scan(&origPath); err != nil {
				if err == sql.ErrNoRows {
					return fmt.Errorf("No repository by ID %d found for this user", id)
				}
				return err
			}

			conf := config.ForContext(ctx)
			repoStore, ok := conf.Get("git.sr.ht", "repos")
			if !ok || repoStore == "" {
				panic(fmt.Errorf("Configuration error: [git.sr.ht]repos is unset"))
			}

			repoPath := path.Join(repoStore, "~"+user.Username, name)
			err := os.Rename(origPath, repoPath)
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("A repository with this name already exists.")
			} else if err != nil {
				return err
			}
			moved = true
			query = query.Set(`path`, repoPath)
		}

		row := query.RunWith(tx).QueryRowContext(ctx)
		if err := row.Scan(&repo.ID, &repo.Created, &repo.Updated,
			&repo.Name, &repo.Description, &repo.RawVisibility,
			&repo.UpstreamURL, &repo.Path, &repo.OwnerID); err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("No repository by ID %d found for this user", id)
			}
			return err
		}

		export := path.Join(repo.Path, "git-daemon-export-ok")
		if repo.Visibility() == model.VisibilityPrivate {
			err := os.Remove(export)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		} else {
			_, err := os.Create(export)
			if err != nil {
				return err
			}
		}

		webhooks.DeliverRepoEvent(ctx, model.WebhookEventRepoUpdate, &repo)
		webhooks.DeliverLegacyRepoUpdate(ctx, &repo)
		return nil
	}); err != nil {
		if moved && err != nil {
			err := os.Rename(repoPath, origPath)
			if err != nil {
				panic(err)
			}
		}
		return nil, err
	}

	return &repo, nil
}

func (r *mutationResolver) DeleteRepository(ctx context.Context, id int) (*model.Repository, error) {
	var repo model.Repository

	if err := database.WithTx(ctx, nil, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `
			DELETE FROM repository
			WHERE id = $1 AND owner_id = $2
			RETURNING
				id, created, updated, name, description, visibility,
				upstream_uri, path, owner_id;
		`, id, auth.ForContext(ctx).UserID)

		if err := row.Scan(&repo.ID, &repo.Created, &repo.Updated,
			&repo.Name, &repo.Description, &repo.RawVisibility,
			&repo.UpstreamURL, &repo.Path, &repo.OwnerID); err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("No repository by ID %d found for this user", id)
			}
			return err
		}

		err := os.RemoveAll(repo.Path)
		if err != nil {
			return err
		}

		webhooks.DeliverRepoEvent(ctx, model.WebhookEventRepoDeleted, &repo)
		webhooks.DeliverLegacyRepoDeleted(ctx, &repo)
		return nil
	}); err != nil {
		return nil, err
	}

	return &repo, nil
}

func (r *mutationResolver) UpdateACL(ctx context.Context, repoID int, mode model.AccessMode, entity string) (*model.ACL, error) {
	if len(entity) == 0 || entity[0] != '~' {
		return nil, fmt.Errorf("Unknown entity '%s'", entity)
	}
	entity = entity[1:]

	if entity == auth.ForContext(ctx).Username {
		return nil, fmt.Errorf("Cannot edit your own access modes")
	}

	var acl model.ACL
	if err := database.WithTx(ctx, nil, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `
			WITH grantee AS (
				SELECT u.id uid, repo.id rid
				FROM "user" u, repository repo
				WHERE u.username = $3 AND repo.id = $1 AND repo.owner_id = $2
			)
			INSERT INTO access (created, updated, mode, user_id, repo_id)
			SELECT
				NOW() at time zone 'utc', NOW() at time zone 'utc',
				$4, grantee.uid, grantee.rid
			FROM grantee
			ON CONFLICT ON CONSTRAINT uq_access_user_id_repo_id
			DO UPDATE SET mode = $4, updated = NOW() at time zone 'utc'
			RETURNING id, created, mode, repo_id, user_id;`,
			repoID, auth.ForContext(ctx).UserID,
			entity, strings.ToLower(string(mode)))
		if err := row.Scan(&acl.ID, &acl.Created, &acl.RawAccessMode,
			&acl.RepoID, &acl.UserID); err != nil {
			if err == sql.ErrNoRows {
				// TODO: Fetch user details from meta.sr.ht
				return fmt.Errorf("No such repository or user found")
			}
			return err
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return &acl, nil
}

func (r *mutationResolver) DeleteACL(ctx context.Context, id int) (*model.ACL, error) {
	var acl model.ACL
	if err := database.WithTx(ctx, nil, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `
			DELETE FROM access
			USING repository repo
			WHERE repo_id = repo.id AND repo.owner_id = $1 AND access.id = $2
			RETURNING access.id, access.created, mode, repo_id, user_id;
		`, auth.ForContext(ctx).UserID, id)
		if err := row.Scan(&acl.ID, &acl.Created, &acl.RawAccessMode,
			&acl.RepoID, &acl.UserID); err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("No such repository or ACL entry found")
			}
			return err
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return &acl, nil
}

func (r *mutationResolver) UploadArtifact(ctx context.Context, repoID int, revspec string, file graphql.Upload) (*model.Artifact, error) {
	conf := config.ForContext(ctx)
	upstream, _ := conf.Get("objects", "s3-upstream")
	accessKey, _ := conf.Get("objects", "s3-access-key")
	secretKey, _ := conf.Get("objects", "s3-secret-key")
	bucket, _ := conf.Get("git.sr.ht", "s3-bucket")
	prefix, _ := conf.Get("git.sr.ht", "s3-prefix")
	if upstream == "" || accessKey == "" || secretKey == "" || bucket == "" {
		return nil, fmt.Errorf("Object storage is not enabled for this server")
	}

	mc, err := minio.New(upstream, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: true,
	})
	if err != nil {
		panic(err)
	}

	repo, err := loaders.ForContext(ctx).RepositoriesByID.Load(repoID)
	if err != nil {
		return nil, fmt.Errorf("Repository %d not found", repoID)
	}
	if repo.OwnerID != auth.ForContext(ctx).UserID {
		return nil, fmt.Errorf("Access denied for repo %d", repoID)
	}

	gitRepo := repo.Repo()
	gitRepo.Lock()
	hash, err := gitRepo.ResolveRevision(plumbing.Revision(revspec))
	gitRepo.Unlock()
	if err != nil {
		return nil, err
	}

	s3path := path.Join(prefix, "artifacts",
		"~"+auth.ForContext(ctx).Username, repo.Name, file.Filename)

	err = mc.MakeBucket(ctx, bucket, minio.MakeBucketOptions{})
	if s3err, ok := err.(minio.ErrorResponse); !ok ||
		(s3err.Code != "BucketAlreadyExists" && s3err.Code != "BucketAlreadyOwnedByYou") {
		panic(err)
	}

	core := minio.Core{mc}
	uid, err := core.NewMultipartUpload(ctx, bucket,
		s3path, minio.PutObjectOptions{
			ContentType: "application/octet-stream",
		})
	if err != nil {
		return nil, err
	}

	defer func() {
		if err := recover(); err != nil {
			core.AbortMultipartUpload(context.Background(), bucket, s3path, uid)
			panic(err)
		}
	}()

	var artifact model.Artifact
	if err := database.WithTx(ctx, nil, func(tx *sql.Tx) error {
		// To guarantee atomicity, we do the following:
		// 1. Upload all parts as an S3 multipart upload, but don't complete
		//    it. This computes the SHA-256 as a side-effect.
		// 2. Insert row into the database and let PostgreSQL prevent
		//    duplicates with constraints. Once this finishes we know we are the
		//    exclusive writer of this artifact.
		// 3. Complete the S3 multi-part upload.
		sha := sha256.New()
		reader := io.TeeReader(file.File, sha)

		var (
			partNum int = 1
			parts   []minio.CompletePart
		)
		for {
			var data [134217728]byte // 128 MiB
			n, err := reader.Read(data[:])
			if errors.Is(err, io.EOF) {
				if n == 0 {
					break
				}
				// Write data[:n] and go for another loop before quitting
			} else if err != nil {
				return err
			}

			md5sum := md5.Sum(data[:n])
			md5b64 := base64.StdEncoding.EncodeToString(md5sum[:])
			shasum := sha256.Sum256(data[:n])
			shahex := hex.EncodeToString(shasum[:])

			opart, err := core.PutObjectPart(ctx, bucket, s3path,
				uid, partNum, bytes.NewReader(data[:n]), int64(n),
				md5b64, shahex, nil)
			if err != nil {
				return err
			}
			parts = append(parts, minio.CompletePart{
				PartNumber: partNum,
				ETag:       opart.ETag,
			})
			partNum++
		}

		checksum := "sha256:" + hex.EncodeToString(sha.Sum(nil))
		row := tx.QueryRowContext(ctx, `
			INSERT INTO artifacts (
				created, user_id, repo_id, commit, filename, checksum, size
			) VALUES (
				NOW() at time zone 'utc',
				$1, $2, $3, $4, $5, $6
			) RETURNING id, created, filename, checksum, size, commit`,
			repo.OwnerID, repo.ID, hash.String(), file.Filename,
			checksum, file.Size)
		if err := row.Scan(&artifact.ID, &artifact.Created, &artifact.Filename,
			&artifact.Checksum, &artifact.Size, &artifact.Commit); err != nil {
			if err.Error() == "pq: duplicate key value violates unique constraint \"repo_artifact_filename_unique\"" {
				return fmt.Errorf("An artifact by this name already exists for this repository (artifact names must be unique for within each repository)")
			}
			return err
		}

		_, err := core.CompleteMultipartUpload(ctx, bucket, s3path, uid, parts)
		return err
	}); err != nil {
		if err != nil {
			core.AbortMultipartUpload(context.Background(), bucket, s3path, uid)
		}
		return nil, err
	}

	return &artifact, nil
}

func (r *mutationResolver) DeleteArtifact(ctx context.Context, id int) (*model.Artifact, error) {
	conf := config.ForContext(ctx)
	upstream, _ := conf.Get("objects", "s3-upstream")
	accessKey, _ := conf.Get("objects", "s3-access-key")
	secretKey, _ := conf.Get("objects", "s3-secret-key")
	bucket, _ := conf.Get("git.sr.ht", "s3-bucket")
	prefix, _ := conf.Get("git.sr.ht", "s3-prefix")
	if upstream == "" || accessKey == "" || secretKey == "" || bucket == "" {
		return nil, fmt.Errorf("Object storage is not enabled for this server")
	}

	mc, err := minio.New(upstream, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: true,
	})
	if err != nil {
		panic(err)
	}

	var artifact model.Artifact
	if err := database.WithTx(ctx, nil, func(tx *sql.Tx) error {
		var repoName string
		row := tx.QueryRowContext(ctx, `
			DELETE FROM artifacts
			USING repository repo
			WHERE
				repo_id = repo.id AND
				repo.owner_id = $1 AND
				artifacts.id = $2
			RETURNING
				artifacts.id, artifacts.created, filename, checksum,
				size, commit, repo.name;`,
			auth.ForContext(ctx).UserID, id)
		if err := row.Scan(&artifact.ID, &artifact.Created, &artifact.Filename,
			&artifact.Checksum, &artifact.Size, &artifact.Commit,
			&repoName); err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("No such artifact for this user")
			}
			return err
		}

		s3path := path.Join(prefix, "artifacts",
			"~"+auth.ForContext(ctx).Username, repoName, artifact.Filename)
		return mc.RemoveObject(ctx, bucket, s3path, minio.RemoveObjectOptions{})
	}); err != nil {
		return nil, err
	}
	return &artifact, nil
}

func (r *mutationResolver) CreateWebhook(ctx context.Context, config model.UserWebhookInput) (model.WebhookSubscription, error) {
	schema := server.ForContext(ctx).Schema
	if err := corewebhooks.Validate(schema, config.Query); err != nil {
		return nil, err
	}

	user := auth.ForContext(ctx)
	ac, err := corewebhooks.NewAuthConfig(ctx)
	if err != nil {
		return nil, err
	}

	var sub model.UserWebhookSubscription
	if len(config.Events) == 0 {
		return nil, fmt.Errorf("Must specify at least one event")
	}
	events := make([]string, len(config.Events))
	for i, ev := range config.Events {
		events[i] = ev.String()
		// TODO: gqlgen does not support doing anything useful with directives
		// on enums at the time of writing, so we have to do a little bit of
		// manual fuckery
		var access string
		switch ev {
		case model.WebhookEventRepoCreated, model.WebhookEventRepoUpdate,
			model.WebhookEventRepoDeleted:
			access = "REPOSITORIES"
		}
		if !user.Grants.Has(access, auth.RO) {
			return nil, fmt.Errorf("Insufficient access granted for webhook event %s", ev.String())
		}
	}

	u, err := url.Parse(config.URL)
	if err != nil {
		return nil, err
	} else if u.Host == "" {
		return nil, fmt.Errorf("Cannot use URL without host")
	} else if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("Cannot use non-HTTP or HTTPS URL")
	}

	if err := database.WithTx(ctx, nil, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `
			INSERT INTO gql_user_wh_sub (
				created, events, url, query,
				auth_method,
				token_hash, grants, client_id, expires,
				node_id,
				user_id
			) VALUES (
				NOW() at time zone 'utc',
				$1, $2, $3, $4, $5, $6, $7, $8, $9, $10
			) RETURNING id, url, query, events, user_id;`,
			pq.Array(events), config.URL, config.Query,
			ac.AuthMethod,
			ac.TokenHash, ac.Grants, ac.ClientID, ac.Expires, // OAUTH2
			ac.NodeID, // INTERNAL
			user.UserID)

		if err := row.Scan(&sub.ID, &sub.URL,
			&sub.Query, pq.Array(&sub.Events), &sub.UserID); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return &sub, nil
}

func (r *mutationResolver) DeleteWebhook(ctx context.Context, id int) (model.WebhookSubscription, error) {
	var sub model.UserWebhookSubscription

	filter, err := corewebhooks.FilterWebhooks(ctx)
	if err != nil {
		return nil, err
	}

	if err := database.WithTx(ctx, nil, func(tx *sql.Tx) error {
		row := sq.Delete(`gql_user_wh_sub`).
			PlaceholderFormat(sq.Dollar).
			Where(sq.And{sq.Expr(`id = ?`, id), filter}).
			Suffix(`RETURNING id, url, query, events, user_id`).
			RunWith(tx).
			QueryRowContext(ctx)
		if err := row.Scan(&sub.ID, &sub.URL,
			&sub.Query, pq.Array(&sub.Events), &sub.UserID); err != nil {
			return err
		}
		return nil
	}); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	return &sub, nil
}

func (r *queryResolver) Version(ctx context.Context) (*model.Version, error) {
	conf := config.ForContext(ctx)
	upstream, _ := conf.Get("objects", "s3-upstream")
	accessKey, _ := conf.Get("objects", "s3-access-key")
	secretKey, _ := conf.Get("objects", "s3-secret-key")
	bucket, _ := conf.Get("git.sr.ht", "s3-bucket")
	artifacts := upstream != "" && accessKey != "" && secretKey != "" && bucket != ""

	sshUser, _ := conf.Get("git.sr.ht::dispatch", "/usr/bin/gitsrht-keys")
	sshUser = strings.Split(sshUser, ":")[0]

	return &model.Version{
		Major:           0,
		Minor:           0,
		Patch:           0,
		DeprecationDate: nil,

		Features: &model.Features{
			Artifacts: artifacts,
		},

		Settings: &model.Settings{
			SSHUser: sshUser,
		},
	}, nil
}

func (r *queryResolver) Me(ctx context.Context) (*model.User, error) {
	user := auth.ForContext(ctx)
	return &model.User{
		ID:       user.UserID,
		Created:  user.Created,
		Updated:  user.Updated,
		Username: user.Username,
		Email:    user.Email,
		URL:      user.URL,
		Location: user.Location,
		Bio:      user.Bio,
	}, nil
}

func (r *queryResolver) User(ctx context.Context, username string) (*model.User, error) {
	return loaders.ForContext(ctx).UsersByName.Load(username)
}

func (r *queryResolver) Repositories(ctx context.Context, cursor *coremodel.Cursor, filter *coremodel.Filter) (*model.RepositoryCursor, error) {
	if cursor == nil {
		cursor = coremodel.NewCursor(filter)
	}

	var repos []*model.Repository
	if err := database.WithTx(ctx, &sql.TxOptions{
		Isolation: 0,
		ReadOnly:  true,
	}, func(tx *sql.Tx) error {
		repo := (&model.Repository{}).As(`repo`)
		query := database.
			Select(ctx, repo).
			From(`repository repo`).
			Where(`repo.owner_id = ?`, auth.ForContext(ctx).UserID)
		repos, cursor = repo.QueryWithCursor(ctx, tx, query, cursor)
		return nil
	}); err != nil {
		return nil, err
	}

	return &model.RepositoryCursor{repos, cursor}, nil
}

func (r *queryResolver) UserWebhooks(ctx context.Context, cursor *coremodel.Cursor) (*model.WebhookSubscriptionCursor, error) {
	if cursor == nil {
		cursor = coremodel.NewCursor(nil)
	}

	filter, err := corewebhooks.FilterWebhooks(ctx)
	if err != nil {
		return nil, err
	}

	var subs []model.WebhookSubscription
	if err := database.WithTx(ctx, &sql.TxOptions{
		Isolation: 0,
		ReadOnly:  true,
	}, func(tx *sql.Tx) error {
		sub := (&model.UserWebhookSubscription{}).As(`sub`)
		query := database.
			Select(ctx, sub).
			From(`gql_user_wh_sub sub`).
			Where(filter)
		subs, cursor = sub.QueryWithCursor(ctx, tx, query, cursor)
		return nil
	}); err != nil {
		return nil, err
	}

	return &model.WebhookSubscriptionCursor{subs, cursor}, nil
}

func (r *queryResolver) UserWebhook(ctx context.Context, id int) (model.WebhookSubscription, error) {
	var sub model.UserWebhookSubscription

	filter, err := corewebhooks.FilterWebhooks(ctx)
	if err != nil {
		return nil, err
	}

	if err := database.WithTx(ctx, &sql.TxOptions{
		Isolation: 0,
		ReadOnly:  true,
	}, func(tx *sql.Tx) error {
		row := database.
			Select(ctx, &sub).
			From(`gql_user_wh_sub`).
			Where(sq.And{sq.Expr(`id = ?`, id), filter}).
			RunWith(tx).
			QueryRowContext(ctx)
		if err := row.Scan(database.Scan(ctx, &sub)...); err != nil {
			return err
		}
		return nil
	}); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	return &sub, nil
}

func (r *queryResolver) Webhook(ctx context.Context) (model.WebhookPayload, error) {
	raw, err := corewebhooks.Payload(ctx)
	if err != nil {
		return nil, err
	}
	payload, ok := raw.(model.WebhookPayload)
	if !ok {
		panic("Invalid webhook payload context")
	}
	return payload, nil
}

func (r *referenceResolver) Artifacts(ctx context.Context, obj *model.Reference, cursor *coremodel.Cursor) (*model.ArtifactCursor, error) {
	// XXX: This could utilize a loader if it ever becomes a bottleneck
	if cursor == nil {
		cursor = coremodel.NewCursor(nil)
	}

	repo := obj.Repo.Repo()
	repo.Lock()
	defer repo.Unlock()
	ref, err := repo.Reference(obj.Ref.Name(), true)
	if err != nil {
		return nil, err
	}
	o, err := repo.Object(plumbing.TagObject, ref.Hash())
	if err == plumbing.ErrObjectNotFound {
		return &model.ArtifactCursor{nil, cursor}, nil
	} else if err != nil {
		return nil, err
	}
	tag, ok := o.(*object.Tag)
	if !ok {
		panic(fmt.Errorf("Expected artifact to be attached to tag"))
	}

	var arts []*model.Artifact
	if err := database.WithTx(ctx, &sql.TxOptions{
		Isolation: 0,
		ReadOnly:  true,
	}, func(tx *sql.Tx) error {
		artifact := (&model.Artifact{}).As(`art`)
		query := database.
			Select(ctx, artifact).
			From(`artifacts art`).
			Where(`art.repo_id = ?`, obj.Repo.ID).
			Where(`art.commit = ?`, tag.Target.String())
		arts, cursor = artifact.QueryWithCursor(ctx, tx, query, cursor)
		return nil
	}); err != nil {
		return nil, err
	}

	return &model.ArtifactCursor{arts, cursor}, nil
}

func (r *repositoryResolver) Owner(ctx context.Context, obj *model.Repository) (model.Entity, error) {
	return loaders.ForContext(ctx).UsersByID.Load(obj.OwnerID)
}

func (r *repositoryResolver) AccessControlList(ctx context.Context, obj *model.Repository, cursor *coremodel.Cursor) (*model.ACLCursor, error) {
	if cursor == nil {
		cursor = coremodel.NewCursor(nil)
	}

	var acls []*model.ACL
	if err := database.WithTx(ctx, &sql.TxOptions{
		Isolation: 0,
		ReadOnly:  true,
	}, func(tx *sql.Tx) error {
		acl := (&model.ACL{}).As(`acl`)
		query := database.
			Select(ctx, acl).
			From(`access acl`).
			Join(`repository repo ON acl.repo_id = repo.id`).
			Where(`acl.repo_id = ?`, obj.ID).
			Where(`repo.owner_id = ?`, auth.ForContext(ctx).UserID)
		acls, cursor = acl.QueryWithCursor(ctx, tx, query, cursor)
		return nil
	}); err != nil {
		return nil, err
	}

	return &model.ACLCursor{acls, cursor}, nil
}

func (r *repositoryResolver) Objects(ctx context.Context, obj *model.Repository, ids []string) ([]model.Object, error) {
	var objects []model.Object
	for _, id := range ids {
		hash := plumbing.NewHash(id)
		o, err := model.LookupObject(obj.Repo(), hash)
		if err != nil {
			return nil, err
		}
		objects = append(objects, o)
	}
	return objects, nil
}

func (r *repositoryResolver) References(ctx context.Context, obj *model.Repository, cursor *coremodel.Cursor) (*model.ReferenceCursor, error) {
	repo := obj.Repo()
	repo.Lock()
	defer repo.Unlock()
	iter, err := repo.References()
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	if cursor == nil {
		cursor = coremodel.NewCursor(nil)
	}

	var refs []*model.Reference
	iter.ForEach(func(ref *plumbing.Reference) error {
		refs = append(refs, &model.Reference{obj, ref})
		return nil
	})

	sort.SliceStable(refs, func(i, j int) bool {
		return refs[i].Name() < refs[j].Name()
	})

	if cursor.Next != "" {
		i := sort.Search(len(refs), func(n int) bool {
			return refs[n].Name() > cursor.Next
		})
		if i != len(refs) {
			refs = refs[i+1:]
		} else {
			refs = nil
		}
	}

	if len(refs) > cursor.Count {
		cursor = &coremodel.Cursor{
			Count:  cursor.Count,
			Next:   refs[cursor.Count].Name(),
			Search: cursor.Search,
		}
		refs = refs[:cursor.Count]
	} else {
		cursor = nil
	}

	return &model.ReferenceCursor{refs, cursor}, nil
}

func (r *repositoryResolver) Log(ctx context.Context, obj *model.Repository, cursor *coremodel.Cursor, from *string) (*model.CommitCursor, error) {
	if cursor == nil {
		cursor = coremodel.NewCursor(nil)
		if from != nil {
			cursor.Next = *from
		}
	}

	repo := obj.Repo()
	opts := &git.LogOptions{
		Order: git.LogOrderCommitterTime,
	}
	if cursor.Next != "" {
		repo.Lock()
		rev, err := repo.ResolveRevision(plumbing.Revision(cursor.Next))
		repo.Unlock()
		if err != nil {
			return nil, err
		}
		if rev == nil {
			return nil, fmt.Errorf("No such revision")
		}
		opts.From = *rev
	}

	repo.Lock()
	log, err := repo.Log(opts)
	repo.Unlock()
	if err != nil {
		return nil, err
	}

	var commits []*model.Commit
	log.ForEach(func(c *object.Commit) error {
		commits = append(commits, model.CommitFromObject(repo, c))
		if len(commits) == cursor.Count+1 {
			return storer.ErrStop
		}
		return nil
	})

	if len(commits) > cursor.Count {
		cursor = &coremodel.Cursor{
			Count:  cursor.Count,
			Next:   commits[cursor.Count].ID,
			Search: "",
		}
		commits = commits[:cursor.Count]
	} else {
		cursor = nil
	}

	return &model.CommitCursor{commits, cursor}, nil
}

func (r *repositoryResolver) Path(ctx context.Context, obj *model.Repository, revspec *string, path string) (*model.TreeEntry, error) {
	rev := plumbing.Revision("HEAD")
	if revspec != nil {
		rev = plumbing.Revision(*revspec)
	}
	repo := obj.Repo()
	repo.Lock()
	hash, err := repo.ResolveRevision(rev)
	repo.Unlock()
	if err != nil {
		return nil, err
	}
	if hash == nil {
		return nil, fmt.Errorf("No such object")
	}
	repo.Lock()
	defer repo.Unlock()
	o, err := repo.Object(plumbing.CommitObject, *hash)
	if err != nil {
		return nil, err
	}
	var (
		commit *object.Commit
		tree   *model.Tree
	)
	commit, _ = o.(*object.Commit)
	if treeObj, err := commit.Tree(); err != nil {
		panic(err)
	} else {
		tree = model.TreeFromObject(repo, treeObj)
	}
	return tree.Entry(path), nil
}

func (r *repositoryResolver) RevparseSingle(ctx context.Context, obj *model.Repository, revspec string) (*model.Commit, error) {
	rev := plumbing.Revision(revspec)
	repo := obj.Repo()
	repo.Lock()
	hash, err := repo.ResolveRevision(rev)
	repo.Unlock()
	if err != nil {
		return nil, err
	}
	if hash == nil {
		return nil, fmt.Errorf("No such object")
	}
	o, err := model.LookupObject(repo, *hash)
	if err != nil {
		return nil, err
	}
	commit, _ := o.(*model.Commit)
	return commit, nil
}

func (r *treeResolver) Entries(ctx context.Context, obj *model.Tree, cursor *coremodel.Cursor) (*model.TreeEntryCursor, error) {
	if cursor == nil {
		// TODO: Filter?
		cursor = coremodel.NewCursor(nil)
	}

	entries := obj.GetEntries()

	if cursor.Next != "" {
		i := sort.Search(len(entries), func(n int) bool {
			return entries[n].Name > cursor.Next
		})
		if i != len(entries) {
			entries = entries[i+1:]
		} else {
			entries = nil
		}
	}

	if len(entries) > cursor.Count {
		cursor = &coremodel.Cursor{
			Count:  cursor.Count,
			Next:   entries[cursor.Count].Name,
			Search: cursor.Search,
		}
		entries = entries[:cursor.Count]
	} else {
		cursor = nil
	}

	return &model.TreeEntryCursor{entries, cursor}, nil
}

func (r *userResolver) Repository(ctx context.Context, obj *model.User, name string) (*model.Repository, error) {
	// TODO: Load repository with user ID instead of username. Needs a new loader.
	return loaders.ForContext(ctx).RepositoriesByOwnerRepoName.Load([2]string{obj.Username, name})
}

func (r *userResolver) Repositories(ctx context.Context, obj *model.User, cursor *coremodel.Cursor, filter *coremodel.Filter) (*model.RepositoryCursor, error) {
	if cursor == nil {
		cursor = coremodel.NewCursor(filter)
	}

	var repos []*model.Repository
	if err := database.WithTx(ctx, &sql.TxOptions{
		Isolation: 0,
		ReadOnly:  true,
	}, func(tx *sql.Tx) error {
		repo := (&model.Repository{}).As(`repo`)
		query := database.
			Select(ctx, repo).
			From(`repository repo`).
			LeftJoin(`access ON repo.id = access.repo_id`).
			Where(sq.And{
				sq.Or{
					sq.Expr(`? IN (access.user_id, repo.owner_id)`,
						auth.ForContext(ctx).UserID),
					sq.Expr(`repo.visibility = 'public'`),
				},
				sq.Expr(`repo.owner_id = ?`, obj.ID),
			})
		repos, cursor = repo.QueryWithCursor(ctx, tx, query, cursor)
		return nil
	}); err != nil {
		return nil, err
	}
	return &model.RepositoryCursor{repos, cursor}, nil
}

func (r *userWebhookSubscriptionResolver) Client(ctx context.Context, obj *model.UserWebhookSubscription) (*model.OAuthClient, error) {
	if obj.ClientID == nil {
		return nil, nil
	}
	return &model.OAuthClient{
		UUID: *obj.ClientID,
	}, nil
}

func (r *userWebhookSubscriptionResolver) Deliveries(ctx context.Context, obj *model.UserWebhookSubscription, cursor *coremodel.Cursor) (*model.WebhookDeliveryCursor, error) {
	if cursor == nil {
		cursor = coremodel.NewCursor(nil)
	}

	var deliveries []*model.WebhookDelivery
	if err := database.WithTx(ctx, &sql.TxOptions{
		Isolation: 0,
		ReadOnly:  true,
	}, func(tx *sql.Tx) error {
		d := (&model.WebhookDelivery{}).
			WithName(`profile`).
			As(`delivery`)
		query := database.
			Select(ctx, d).
			From(`gql_user_wh_delivery delivery`).
			Where(`delivery.subscription_id = ?`, obj.ID)
		deliveries, cursor = d.QueryWithCursor(ctx, tx, query, cursor)
		return nil
	}); err != nil {
		return nil, err
	}

	return &model.WebhookDeliveryCursor{deliveries, cursor}, nil
}

func (r *userWebhookSubscriptionResolver) Sample(ctx context.Context, obj *model.UserWebhookSubscription, event *model.WebhookEvent) (string, error) {
	// TODO
	panic(fmt.Errorf("not implemented"))
}

func (r *webhookDeliveryResolver) Subscription(ctx context.Context, obj *model.WebhookDelivery) (model.WebhookSubscription, error) {
	if obj.Name == "" {
		panic("WebhookDelivery without name")
	}

	// XXX: This could use a loader but it's unlikely to be a bottleneck
	var sub model.WebhookSubscription
	if err := database.WithTx(ctx, &sql.TxOptions{
		Isolation: 0,
		ReadOnly:  true,
	}, func(tx *sql.Tx) error {
		// XXX: This needs some work to generalize to other kinds of webhooks
		subscription := (&model.UserWebhookSubscription{}).As(`sub`)
		// Note: No filter needed because, if we have access to the delivery,
		// we also have access to the subscription.
		row := database.
			Select(ctx, subscription).
			From(`gql_user_wh_sub sub`).
			Where(`sub.id = ?`, obj.SubscriptionID).
			RunWith(tx).
			QueryRowContext(ctx)
		if err := row.Scan(database.Scan(ctx, subscription)...); err != nil {
			return err
		}
		sub = subscription
		return nil
	}); err != nil {
		return nil, err
	}
	return sub, nil
}

// ACL returns api.ACLResolver implementation.
func (r *Resolver) ACL() api.ACLResolver { return &aCLResolver{r} }

// Artifact returns api.ArtifactResolver implementation.
func (r *Resolver) Artifact() api.ArtifactResolver { return &artifactResolver{r} }

// Commit returns api.CommitResolver implementation.
func (r *Resolver) Commit() api.CommitResolver { return &commitResolver{r} }

// Mutation returns api.MutationResolver implementation.
func (r *Resolver) Mutation() api.MutationResolver { return &mutationResolver{r} }

// Query returns api.QueryResolver implementation.
func (r *Resolver) Query() api.QueryResolver { return &queryResolver{r} }

// Reference returns api.ReferenceResolver implementation.
func (r *Resolver) Reference() api.ReferenceResolver { return &referenceResolver{r} }

// Repository returns api.RepositoryResolver implementation.
func (r *Resolver) Repository() api.RepositoryResolver { return &repositoryResolver{r} }

// Tree returns api.TreeResolver implementation.
func (r *Resolver) Tree() api.TreeResolver { return &treeResolver{r} }

// User returns api.UserResolver implementation.
func (r *Resolver) User() api.UserResolver { return &userResolver{r} }

// UserWebhookSubscription returns api.UserWebhookSubscriptionResolver implementation.
func (r *Resolver) UserWebhookSubscription() api.UserWebhookSubscriptionResolver {
	return &userWebhookSubscriptionResolver{r}
}

// WebhookDelivery returns api.WebhookDeliveryResolver implementation.
func (r *Resolver) WebhookDelivery() api.WebhookDeliveryResolver { return &webhookDeliveryResolver{r} }

type aCLResolver struct{ *Resolver }
type artifactResolver struct{ *Resolver }
type commitResolver struct{ *Resolver }
type mutationResolver struct{ *Resolver }
type queryResolver struct{ *Resolver }
type referenceResolver struct{ *Resolver }
type repositoryResolver struct{ *Resolver }
type treeResolver struct{ *Resolver }
type userResolver struct{ *Resolver }
type userWebhookSubscriptionResolver struct{ *Resolver }
type webhookDeliveryResolver struct{ *Resolver }
