from collections import deque
from datetime import datetime, timedelta, timezone
from pygit2 import Repository as GitRepository, Tag
from jinja2 import Markup, escape
from stat import filemode
import pygit2
import json

def trim_commit(msg):
    if "\n" not in msg:
        return msg
    return msg[:msg.index("\n")]

def commit_time(commit):
    author = commit.author if hasattr(commit, 'author') else commit.tagger
    # Time handling in python is so dumb
    try:
        tzinfo = timezone(timedelta(minutes=author.offset))
        tzaware = datetime.fromtimestamp(float(author.time), tzinfo)
        diff = datetime.now(timezone.utc) - tzaware
        return datetime.utcnow() - diff
    except:
        return datetime.utcnow()

def _get_ref(repo, ref):
    return repo._get(ref)

def diff_for_commit(git_repo, commit):
    try:
        parent = git_repo.revparse_single(commit.id.hex + "^")
        diff = git_repo.diff(parent, commit)
    except KeyError:
        parent = None
        diff = commit.tree.diff_to_tree(swap=True)
    diff.find_similar(pygit2.GIT_DIFF_FIND_RENAMES)
    return parent, diff

def get_log(git_repo, commit, path="", commits_per_page=20, until=None):
    commits = list()
    for commit in git_repo.walk(commit.id, pygit2.GIT_SORT_TOPOLOGICAL):
        if path:
            _, diff = diff_for_commit(git_repo, commit)
            for patch in diff:
                exact = False
                if patch.delta.new_file.path == path:
                    path = patch.delta.old_file.path
                    exact = True
                if exact or patch.delta.new_file.path.startswith(path + "/"):
                    commits.append(commit)
                    break
        else:
            commits.append(commit)

        if until is not None and commit == until:
            break
        elif len(commits) >= commits_per_page + 1:
            break
    return commits

class Repository(GitRepository):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)

    def __enter__(self):
        return self

    def __exit__(self, *_):
        self.free()

    def get(self, ref):
        return _get_ref(self, ref)

    def _get(self, ref):
        return super().get(ref)

    def default_branch(self):
        head_ref = self.lookup_reference("HEAD")
        return self.branches.get(head_ref.target[len("refs/heads/"):])

    def default_branch_name(self):
        branch = self.default_branch()
        if branch:
            return branch.name[len("refs/heads/"):]
        else:
            return None

    @property
    def is_empty(self):
        return len(list(self.branches.local)) == 0

class AnnotatedTreeEntry:
    def __init__(self, repo, entry):
        self._entry = entry
        self._repo = repo
        self.commit = None
        if entry:
            self.id = entry.id.hex
            self.name = entry.name
            self.type = (entry.type_str
                    if hasattr(entry, "type_str") else entry.type)
            self.filemode = entry.filemode

    def fetch_blob(self):
        if self.type == "tree":
            self.tree = self._repo.get(self.id)
        else:
            self.blob = self._repo.get(self.id)
        return self

    def serialize(self):
        return {
            "id": self.id,
            "name": self.name,
            "type": self.type,
            "filemode": self.filemode,
            "commit": (self.commit.id.hex
                if hasattr(self, "commit") and self.commit else None),
        }

    @staticmethod
    def deserialize(res, repo):
        _id = res["id"]
        self = AnnotatedTreeEntry(repo, None)
        self.id = res["id"]
        self.name = res["name"]
        self.type = res["type"]
        self.filemode = res["filemode"]
        self.commit = repo.get(res["commit"]) if "commit" in res else None
        return self

    def __hash__(self):
        return hash(f"{self.id}:{self.name}")

    def __eq__(self, other):
        return self.id == other.id and self.name == other.name

    def __repr__(self):
        return f"<AnnotatedTreeEntry {self.name} {self.id}>"

def annotate_tree(repo, tree, commit):
    return [AnnotatedTreeEntry(repo, entry).fetch_blob() for entry in tree]

def _diffstat_name(delta, anchor):
    if delta.status == pygit2.GIT_DELTA_DELETED:
        return Markup(escape(delta.old_file.path))
    if delta.old_file.path == delta.new_file.path:
        return Markup(
                f"<a href='#{escape(anchor)}{escape(delta.old_file.path)}'>" +
                f"{escape(delta.old_file.path)}" +
                f"</a>")
    # Based on git/diff.c
    pfx_length = 0
    old_path = delta.old_file.path
    new_path = delta.new_file.path
    for i in range(max(len(old_path), len(new_path))):
        if i >= len(old_path) or i >= len(new_path):
            break
        if old_path[i] == '/':
            pfx_length = i + 1
    # TODO: detect common suffix
    if pfx_length != 0:
        return (f"{delta.old_file.path[:pfx_length]}{{" +
            f"{delta.old_file.path[pfx_length:]} =&gt; {delta.new_file.path[pfx_length:]}" +
            f"}}")
    return f"{delta.old_file.path} => {delta.new_file.path}"

def _diffstat_line(delta, patch, anchor):
    name = _diffstat_name(delta, anchor)
    change = ""
    if delta.status not in [
                pygit2.GIT_DELTA_ADDED,
                pygit2.GIT_DELTA_DELETED,
            ]:
        if delta.old_file.mode != delta.new_file.mode:
            change = Markup(
                f" <span title='{delta.old_file.mode}'>" +
                f"{filemode(delta.old_file.mode)}</span> => " +
                f"<span title='{delta.new_file.mode}'>" +
                f"{filemode(delta.new_file.mode)}</span>")
    return Markup(f"{delta.status_char()} {name}{change}\n")

def diffstat(diff, anchor=""):
    stat = Markup(f"""{diff.stats.files_changed} files changed, <strong
        class="text-success">{diff.stats.insertions
        }</strong> insertions(+), <strong
        class="text-danger">{diff.stats.deletions
        }</strong> deletions(-)\n\n""")
    for delta, patch in zip(diff.deltas, diff):
        stat += _diffstat_line(delta, patch, anchor)
    return stat
