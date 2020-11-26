package graph

// This file will be automatically regenerated based on the schema, any resolver implementations
// will be copied through when generating and any unknown code will be moved to the end.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	"git.sr.ht/~sircmpwn/core-go/auth"
	"git.sr.ht/~sircmpwn/core-go/config"
	"git.sr.ht/~sircmpwn/core-go/database"
	coremodel "git.sr.ht/~sircmpwn/core-go/model"
	"git.sr.ht/~sircmpwn/git.sr.ht/api/graph/api"
	"git.sr.ht/~sircmpwn/git.sr.ht/api/graph/model"
	"git.sr.ht/~sircmpwn/git.sr.ht/api/loaders"
	"github.com/99designs/gqlgen/graphql"
	sq "github.com/Masterminds/squirrel"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
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

func (r *mutationResolver) CreateRepository(ctx context.Context, name string, visibility model.Visibility, description *string) (*model.Repository, error) {
	if !repoNameRE.MatchString(name) {
		return nil, fmt.Errorf("Invalid repository name '%s' (must match %s)",
			name, repoNameRE.String())
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
			&repo.Description, &repo.Visibility, &repo.UpstreamURL, &repo.Path,
			&repo.OwnerID); err != nil {
			if strings.Contains(err.Error(), "duplicate key value violates unique constraint") {
				return fmt.Errorf("A repository with this name already exists.")
			}
			return err
		}

		gitrepo, err := git.PlainInit(repoPath, true)
		if err != nil {
			return err
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
	if err := database.WithTx(ctx, nil, func(tx *sql.Tx) error {
		user := auth.ForContext(ctx)

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
			&repo.Name, &repo.Description, &repo.Visibility,
			&repo.UpstreamURL, &repo.Path, &repo.OwnerID); err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("No repository by ID %d found for this user", id)
			}
			return err
		}
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
	panic(fmt.Errorf("deleteRepository: not implemented"))
}

func (r *mutationResolver) UpdateACL(ctx context.Context, repoID int, mode model.AccessMode, entity string) (*model.ACL, error) {
	panic(fmt.Errorf("updateACL: not implemented"))
}

func (r *mutationResolver) DeleteACL(ctx context.Context, repoID int, entity string) (*model.ACL, error) {
	panic(fmt.Errorf("deleteACL: not implemented"))
}

func (r *mutationResolver) UploadArtifact(ctx context.Context, repoID int, revspec string, file graphql.Upload) (*model.Artifact, error) {
	panic(fmt.Errorf("uploadArtifact: not implemented"))
}

func (r *mutationResolver) DeleteArtifact(ctx context.Context, id int) (*model.Artifact, error) {
	panic(fmt.Errorf("deleteArtifact: not implemented"))
}

func (r *queryResolver) Version(ctx context.Context) (*model.Version, error) {
	return &model.Version{
		Major:           0,
		Minor:           0,
		Patch:           0,
		DeprecationDate: nil,
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

func (r *queryResolver) Repository(ctx context.Context, id int) (*model.Repository, error) {
	return loaders.ForContext(ctx).RepositoriesByID.Load(id)
}

func (r *queryResolver) RepositoryByName(ctx context.Context, name string) (*model.Repository, error) {
	return loaders.ForContext(ctx).RepositoriesByName.Load(name)
}

func (r *queryResolver) RepositoryByOwner(ctx context.Context, owner string, repo string) (*model.Repository, error) {
	if strings.HasPrefix(owner, "~") {
		owner = owner[1:]
	} else {
		return nil, fmt.Errorf("Expected owner to be a canonical name")
	}
	return loaders.ForContext(ctx).
		RepositoriesByOwnerRepoName.Load([2]string{owner, repo})
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
	o, err := repo.Object(plumbing.CommitObject, *hash)
	repo.Unlock()
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
			Where(`repo.owner_id = ?`, obj.ID)
		repos, cursor = repo.QueryWithCursor(ctx, tx, query, cursor)
		return nil
	}); err != nil {
		return nil, err
	}
	return &model.RepositoryCursor{repos, cursor}, nil
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

type aCLResolver struct{ *Resolver }
type artifactResolver struct{ *Resolver }
type commitResolver struct{ *Resolver }
type mutationResolver struct{ *Resolver }
type queryResolver struct{ *Resolver }
type referenceResolver struct{ *Resolver }
type repositoryResolver struct{ *Resolver }
type treeResolver struct{ *Resolver }
type userResolver struct{ *Resolver }
