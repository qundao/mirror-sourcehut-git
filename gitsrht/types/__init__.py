from srht.database import Base
from srht.oauth import ExternalUserMixin, ExternalOAuthTokenMixin
from scmsrht.repos import (
        BaseAccessMixin, BaseRedirectMixin, BaseRepositoryMixin)

class User(Base, ExternalUserMixin):
    pass

class OAuthToken(Base, ExternalOAuthTokenMixin):
    pass

class Access(Base, BaseAccessMixin):
    pass

class Redirect(Base, BaseRedirectMixin):
    pass

class Repository(Base, BaseRepositoryMixin):
    pass
