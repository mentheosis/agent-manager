import importlib.util
import json
from pathlib import Path
import ssl

import pytest


spec = importlib.util.spec_from_file_location(
    "container_entrypoint", Path(__file__).parents[1] / "scripts/container-entrypoint.py"
)
entrypoint = importlib.util.module_from_spec(spec)
spec.loader.exec_module(entrypoint)


def test_host_auth_import_preserves_refresh_until_host_changes(tmp_path):
    source = tmp_path / "host-auth.json"
    source.write_text(json.dumps({"tokens": {"access_token": "host-token"}}))
    env = {"HOME": str(tmp_path / "home"), "AGENT_MANAGER_CODEX_AUTH_SOURCE": str(source)}
    entrypoint.configure(env)
    target = tmp_path / "home/.codex/auth.json"
    assert target.read_bytes() == source.read_bytes()
    assert target.stat().st_mode & 0o777 == 0o600
    refreshed = json.dumps({"tokens": {"access_token": "refreshed-token"}})
    target.write_text(refreshed)
    entrypoint.configure(env)
    assert target.read_text() == refreshed
    source.write_text(json.dumps({"tokens": {"access_token": "new-host-login"}}))
    entrypoint.configure(env)
    assert target.read_bytes() == source.read_bytes()


def test_invalid_auth_does_not_overwrite_existing_credentials(tmp_path):
    source = tmp_path / "host-auth.json"
    source.write_text("{}")
    target = tmp_path / "codex/auth.json"
    target.parent.mkdir()
    target.write_text("existing")
    with pytest.raises(ValueError, match="no credentials"):
        entrypoint.configure({"HOME": str(tmp_path), "CODEX_HOME": str(target.parent),
                              "AGENT_MANAGER_CODEX_AUTH_SOURCE": str(source)})
    assert target.read_text() == "existing"


def test_extra_ca_preserves_system_trust_and_configures_clients(tmp_path):
    system = Path(ssl.get_default_verify_paths().cafile)
    extra = tmp_path / "extra.pem"
    extra.write_text(ssl.DER_cert_to_PEM_cert(ssl.create_default_context().get_ca_certs(binary_form=True)[0]))
    env = {"HOME": str(tmp_path), "SSL_CERT_FILE": str(system),
           "AGENT_MANAGER_EXTRA_CA_CERTS": str(extra)}
    entrypoint.configure(env)
    bundle = Path(env["SSL_CERT_FILE"])
    assert bundle.read_bytes() == system.read_bytes() + b"\n" + extra.read_bytes()
    assert all(env[name] == str(bundle) for name in
               ("NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"))
    ssl.create_default_context(cafile=str(bundle))


def test_no_host_configuration_is_a_noop(tmp_path):
    env = {"HOME": str(tmp_path)}
    entrypoint.configure(env)
    assert env == {"HOME": str(tmp_path)}
    assert not list(tmp_path.iterdir())
