from srht.config import cfg
from srht.oauth import OAuthScope, AbstractOAuthService, set_base_service
from srht.oauth import meta_delegated_exchange
from srht.flask import DATE_FORMAT
from srht.database import db
from gitsrht.types import OAuthToken, User
from datetime import datetime

client_id = cfg("meta.sr.ht", "oauth-client-id")
client_secret = cfg("meta.sr.ht", "oauth-client-secret")
revocation_url = "{}://{}/oauth/revoke".format(
    cfg("server", "protocol"), cfg("server", "domain"))

class GitOAuthService(AbstractOAuthService):
    def get_client_id(self):
        return client_id

    def get_token(self, token, token_hash, scopes):
        now = datetime.utcnow()
        oauth_token = (OAuthToken.query
                .filter(OAuthToken.token_hash == token_hash)
                .filter(OAuthToken.expires > now)
        ).first()
        if oauth_token:
            return oauth_token
        _token, profile = meta_delegated_exchange(
                token, client_id, client_secret, revocation_url)
        expires = datetime.strptime(_token["expires"], DATE_FORMAT)
        scopes = set(OAuthScope(s) for s in _token["scopes"].split(","))
        user = User.query.filter(User.username == profile["username"]).first()
        if not user:
            user = User()
            user.username = profile.get("username")
            user.email = profile.get("email")
            user.paid = profile.get("paid")
            user.oauth_token = token
            user.oauth_token_expires = expires
            db.session.add(user)
            db.session.flush()
        oauth_token = OAuthToken(user, token, expires)
        oauth_token.scopes = ",".join(str(s) for s in scopes)
        db.session.add(oauth_token)
        db.session.commit()
        return oauth_token

set_base_service(GitOAuthService())