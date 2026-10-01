"""Rendered optional invitation surfaces contain no secret or remote assets."""
from pathlib import Path

from jinja2 import Environment, FileSystemLoader, select_autoescape


TEMPLATES = Path(__file__).resolve().parents[1] / "templates"


def render(name, **changes):
    env = Environment(loader=FileSystemLoader(TEMPLATES), autoescape=select_autoescape(["html"]))
    return env.get_template(name).render(
        identity={"id": "synthetic-user", "display": "Synthetic User", "method": "npm-header"},
        asset_version="synthetic", library_brand="Synthetic Library", app_version="dev",
        app_switcher_script_url="", private_indexer_enabled=True, show_library_stats=False,
        **changes,
    )


def test_invitation_link_is_opt_in_and_in_account_surface():
    disabled = render("library.html", private_invitations_enabled=False)
    enabled = render("library.html", private_invitations_enabled=True)
    assert 'href="/invitations"' not in disabled
    assert 'href="/invitations"' in enabled
    assert enabled.index('id="libAccount"') < enabled.index('href="/invitations"')


def test_management_render_starts_with_payment_actions_hidden_and_disabled():
    html = render("invitations.html")
    assert 'data-account-id="synthetic-user"' in html
    assert 'role="status" aria-live="polite"' in html
    assert 'id="invShareUrl" readonly' in html
    assert 'id="invCreate" type="submit" disabled' in html
    assert 'id="invCheckout" class="inv-primary" type="button" disabled hidden' in html
    assert 'id="invCheckoutRefresh" class="inv-secondary" type="button" disabled hidden' in html
    assert "Purchases are currently unavailable." in html
    assert "https://" not in html
    assert "bi_" not in html


def test_landing_render_never_embeds_token_identity_or_external_script():
    html = render("invite.html")
    assert 'name="referrer" content="no-referrer"' in html
    assert 'data-invitation-view="redeem"' in html
    assert "synthetic-user" not in html
    assert 'id="invAccept" type="button" disabled hidden' in html
    assert 'src="/static/js/invitations.js' in html
    assert "https://" not in html
    assert "bi_" not in html



def test_landing_bridge_is_optional_and_has_only_a_top_level_post_action():
    disabled = render("invite.html", invitation_sign_in_url="")
    assert 'id="invSignInForm"' not in disabled
    html = render("invite.html", invitation_sign_in_url="https://sso.example.test/invitations/start")
    assert 'action="https://sso.example.test/invitations/start" method="post" target="_top" hidden' in html
    assert 'id="invSignInToken" hidden></span>' in html
    assert 'id="invSignIn" type="submit" disabled' in html
    assert 'name="token"' not in html
    assert "synthetic-user" not in html
    assert "bi_" not in html
    assert 'name="returnUrl"' not in html
    assert 'name="provider"' not in html
