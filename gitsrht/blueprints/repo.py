import os
import pygit2
import pygments
import subprocess
from datetime import datetime, timedelta
from jinja2 import Markup
from flask import Blueprint, render_template, abort, send_file, request
from flask_login import current_user
from gitsrht.access import get_repo, has_access, UserAccess
from gitsrht.editorconfig import EditorConfig
from gitsrht.redis import redis
from gitsrht.git import CachedRepository, commit_time, annotate_tree
from gitsrht.types import User, Repository
from io import BytesIO
from pygments import highlight
from pygments.lexers import guess_lexer_for_filename, TextLexer
from pygments.formatters import HtmlFormatter
from srht.config import cfg
from srht.markdown import markdown

repo = Blueprint('repo', __name__)

def get_readme(repo, tip):
    if not tip or not "README.md" in tip.tree:
        return None
    readme = tip.tree["README.md"]
    if readme.type != "blob":
        return None
    key = f"git.sr.ht:git:markdown:{readme.id.hex}"
    html = redis.get(key)
    if html:
        return Markup(html.decode())
    try:
        md = repo.get(readme.id).data.decode()
    except:
        pass
    html = markdown(md, ["h1", "h2", "h3", "h4", "h5"])
    redis.setex(key, html, timedelta(days=7))
    return Markup(html)

def _highlight_file(name, data, blob_id):
    key = f"git.sr.ht:git:highlight:{blob_id}"
    html = redis.get(key)
    if html:
        return Markup(html.decode())
    try:
        lexer = guess_lexer_for_filename(name, data)
    except pygments.util.ClassNotFound:
        lexer = TextLexer()
    formatter = HtmlFormatter()
    style = formatter.get_style_defs('.highlight')
    html = f"<style>{style}</style>" + highlight(data, lexer, formatter)
    redis.setex(key, html, timedelta(days=7))
    return Markup(html)

@repo.route("/<owner>/<repo>")
def summary(owner, repo):
    owner, repo = get_repo(owner, repo)
    if not repo:
        abort(404)
    if not has_access(repo, UserAccess.read):
        abort(401)
    git_repo = CachedRepository(repo.path)
    base = (cfg("git.sr.ht", "origin")
        .replace("http://", "")
        .replace("https://", ""))
    clone_urls = [
        url.format(base, owner.canonical_name, repo.name)
        for url in ["https://{}/{}/{}", "git@{}:{}/{}"]
    ]
    if git_repo.is_empty:
        return render_template("empty-repo.html", owner=owner, repo=repo,
                clone_urls=clone_urls)
    default_branch = git_repo.default_branch()
    tip = git_repo.get(default_branch.target)
    commits = list()
    for commit in git_repo.walk(tip.id, pygit2.GIT_SORT_TIME):
        commits.append(commit)
        if len(commits) >= 3:
            break
    readme = get_readme(git_repo, tip)
    tags = [(ref, git_repo.get(git_repo.references[ref].target))
        for ref in git_repo.listall_references()
        if ref.startswith("refs/tags/")]
    tags = sorted(tags, key=lambda c: commit_time(c[1]), reverse=True)
    latest_tag = tags[0] if len(tags) else None
    return render_template("summary.html", view="summary",
            owner=owner, repo=repo, readme=readme, commits=commits,
            clone_urls=clone_urls, latest_tag=latest_tag,
            default_branch=default_branch)

def resolve_ref(git_repo, ref):
    commit = None
    if ref is None:
        branch = git_repo.default_branch()
        ref = branch.name[len("refs/heads/"):]
        commit = git_repo.get(branch.target)
    else:
        if f"refs/heads/{ref}" in git_repo.references:
            branch = git_repo.references[f"refs/heads/{ref}"]
            commit = git_repo.get(branch.target)
        elif f"refs/tags/{ref}" in git_repo.references:
            _ref = git_repo.references[f"refs/tags/{ref}"]
            tag = git_repo.get(_ref.target)
            commit = git_repo.get(tag.target)
        else:
            try:
                ref = git_repo.get(ref)
            except:
                abort(404)
    if not commit:
        abort(404)
    return ref, commit

@repo.route("/<owner>/<repo>/tree", defaults={"ref": None, "path": ""})
@repo.route("/<owner>/<repo>/tree/<ref>", defaults={"path": ""})
@repo.route("/<owner>/<repo>/tree/<ref>/<path:path>")
def tree(owner, repo, ref, path):
    owner, repo = get_repo(owner, repo)
    if not repo:
        abort(404)
    if not has_access(repo, UserAccess.read):
        abort(401)
    git_repo = CachedRepository(repo.path)
    ref, commit = resolve_ref(git_repo, ref)

    tree = commit.tree
    editorconfig = EditorConfig(git_repo, tree, path)

    path = path.split("/")
    for part in path:
        if part == "":
            continue
        if part not in tree:
            abort(404)
        entry = tree[part]
        if entry.type == "blob":
            tree = annotate_tree(git_repo, tree, commit)
            commit = next(e.commit for e in tree if e.name == entry.name)
            blob = git_repo.get(entry.id)
            data = None
            if not blob.is_binary:
                try:
                    data = blob.data.decode()
                except:
                    pass
            return render_template("blob.html", view="tree",
                    owner=owner, repo=repo, ref=ref, path=path, entry=entry,
                    blob=blob, data=data, commit=commit,
                    highlight_file=_highlight_file,
                    editorconfig=editorconfig)
        tree = git_repo.get(entry.id)

    tree = annotate_tree(git_repo, tree, commit)
    tree = sorted(tree, key=lambda e: e.name)

    return render_template("tree.html", view="tree", owner=owner, repo=repo,
            ref=ref, commit=commit, tree=tree, path=path)

@repo.route("/<owner>/<repo>/blob/<ref>/<path:path>")
def raw_blob(owner, repo, ref, path):
    owner, repo = get_repo(owner, repo)
    if not repo:
        abort(404)
    if not has_access(repo, UserAccess.read):
        abort(401)
    git_repo = CachedRepository(repo.path)
    ref, commit = resolve_ref(git_repo, ref)

    blob = None
    entry = None
    tree = commit.tree
    path = path.split("/")
    for part in path:
        if part == "":
            continue
        if part not in tree:
            abort(404)
        entry = tree[part]
        if entry.type == "blob":
            tree = annotate_tree(git_repo, tree, commit)
            commit = next(e.commit for e in tree if e.name == entry.name)
            blob = git_repo.get(entry.id)
            break
        tree = git_repo.get(entry.id)

    if not blob:
        abort(404)

    return send_file(BytesIO(blob.data),
            as_attachment=blob.is_binary, attachment_filename=entry.name)

@repo.route("/<owner>/<repo>/archive/<ref>")
def archive(owner, repo, ref):
    owner, repo = get_repo(owner, repo)
    if not repo:
        abort(404)
    if not has_access(repo, UserAccess.read):
        abort(401)
    git_repo = CachedRepository(repo.path)
    ref, commit = resolve_ref(git_repo, ref)

    path = f"/tmp/{commit.id.hex}.tar.gz"
    try:
        args = [
            "git",
            "--git-dir", repo.path,
            "archive",
            "--format=tar.gz",
            "--prefix", f"{repo.name}-{ref}"
            "-o", path, ref
        ]
        print(args)
        subp = subprocess.run(args, timeout=30,
                stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    except:
        try:
            os.unlink(path)
        except:
            pass
        return "Error preparing archive", 500

    if subp.returncode != 0:
        print(subp.stdout, subp.stderr)
        try:
            os.unlink(path)
        except:
            pass
        return "Error preparing archive", 500

    f = open(path, "rb")
    os.unlink(path)
    return send_file(f, mimetype="application/tar+gzip", as_attachment=True,
            attachment_filename=f"{repo.name}-{ref}.tar.gz")

class _AnnotatedRef:
    def __init__(self, repo, ref):
        self.ref = ref
        self.target = ref.target
        if ref.name.startswith("refs/heads/"):
            self.type = "branch"
            self.name = ref.name[len("refs/heads/"):]
            self.branch = repo.get(ref.target)
        elif ref.name.startswith("refs/tags/"):
            self.type = "tag"
            self.name = ref.name[len("refs/tags/"):]
            self.tag = repo.get(ref.target)
        else:
            self.type = None

@repo.route("/<owner>/<repo>/log", defaults={"ref": None, "path": ""})
@repo.route("/<owner>/<repo>/log/<ref>", defaults={"path": ""})
@repo.route("/<owner>/<repo>/log/<ref>/<path:path>")
def log(owner, repo, ref, path):
    owner, repo = get_repo(owner, repo)
    if not repo:
        abort(404)
    if not has_access(repo, UserAccess.read):
        abort(401)
    git_repo = CachedRepository(repo.path)
    ref, commit = resolve_ref(git_repo, ref)

    refs = {}
    for _ref in git_repo.references:
        _ref = _AnnotatedRef(git_repo, git_repo.references[_ref])
        if not _ref.type:
            continue
        if _ref.target.hex not in refs:
            refs[_ref.target.hex] = []
        refs[_ref.target.hex].append(_ref)

    from_id = request.args.get("from")
    if from_id:
        commit = git_repo.get(from_id)

    commits_per_page = 20
    commits = list()
    for commit in git_repo.walk(commit.id, pygit2.GIT_SORT_TIME):
        commits.append(commit)
        if len(commits) >= commits_per_page + 1:
            break

    return render_template("log.html", view="log",
            owner=owner, repo=repo, ref=ref, path=path,
            commits=commits, refs=refs)
