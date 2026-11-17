#!/usr/bin/env python3
"""在临时目录验证主机安装流程，不执行目标二进制或修改系统服务。"""
import hashlib
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


REPO = Path(__file__).resolve().parents[1]


class InstallTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="openlb-install-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.package = self.root / "package"
        shutil.copytree(REPO / "deploy/agent", self.package)
        binary = self.package / "bin/openlb-agent"
        binary.parent.mkdir()
        binary.write_text("#!/bin/sh\nexit 0\n")
        (self.package / "SHA256SUMS").write_text(
            hashlib.sha256(binary.read_bytes()).hexdigest() + "  bin/openlb-agent\n"
        )
        self.dest = self.root / "target"
        self.config = self.dest / "etc/nginx/nginx.conf"
        self.expected = (self.package / "nginx/nginx.conf").read_bytes()

    def run_install(self, success=True):
        result = subprocess.run(
            ["sh", str(self.package / "install.sh")],
            env=dict(os.environ, DESTDIR=str(self.dest)),
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)
        return result

    def test_missing_nginx_warns_but_installs(self):
        result = self.run_install()
        self.assertIn("nginx not installed", result.stderr)
        self.assertEqual(self.config.read_bytes(), self.expected)
        self.assertFalse(self.config.with_suffix(".conf.bak").exists())
        self.assertTrue((self.dest / "usr/local/bin/openlb-agent").is_file())
        units = list((self.dest / "etc/systemd/system").glob("*.service"))
        self.assertEqual([unit.name for unit in units], ["openlb-agent.service"])
        self.assertFalse((self.dest / "etc/systemd/system/openlb-agent.service.d").exists())
        self.assertFalse((self.dest / "usr/lib/sysusers.d").exists())

    def test_installed_nginx_is_not_executed_and_config_is_backed_up(self):
        binary = self.dest / "usr/sbin/nginx"
        binary.parent.mkdir(parents=True)
        marker = self.root / "nginx-was-executed"
        binary.write_text(f"#!/bin/sh\ntouch '{marker}'\nexit 99\n")
        binary.chmod(0o755)
        self.config.parent.mkdir(parents=True)
        self.config.write_text("original nginx configuration\n")
        self.config.chmod(0o600)
        result = self.run_install()
        self.assertNotIn("nginx not installed", result.stderr)
        self.assertFalse(marker.exists())
        backup = self.config.with_suffix(".conf.bak")
        self.assertEqual(backup.read_text(), "original nginx configuration\n")
        self.assertEqual(backup.stat().st_mode & 0o777, 0o600)
        self.assertEqual(self.config.read_bytes(), self.expected)

    def test_repeat_install_backs_up_latest_and_preserves_env_and_state(self):
        self.run_install()
        env = self.dest / "etc/openlb/agent.env"
        env.write_text(env.read_text() + "\n# local change\n")
        state = self.dest / "var/lib/openlb-agent/service-locks.json"
        state.write_text('{"preserve":true}')
        listener = self.dest / "etc/nginx/conf.d/http/default_web_http_80.conf"
        listener.write_text("existing listener")
        self.config.write_text("latest custom config")
        self.config.with_suffix(".conf.bak").write_text("older backup")
        saved = {path: path.read_bytes() for path in (env, state, listener)}
        self.run_install()
        self.assertEqual(self.config.with_suffix(".conf.bak").read_text(), "latest custom config")
        self.assertEqual(self.config.read_bytes(), self.expected)
        for path, content in saved.items():
            self.assertEqual(path.read_bytes(), content)

    def test_custom_directory_uses_existing_env(self):
        env = self.dest / "etc/openlb/agent.env"
        env.parent.mkdir(parents=True)
        env.write_text('OPENLB_NGINX_HOME="/srv/nginx custom"\nOPENLB_NGINX_BIN=/missing/nginx\n')
        config = self.dest / "srv/nginx custom/nginx.conf"
        config.parent.mkdir(parents=True)
        config.write_text("custom config")
        self.run_install()
        self.assertEqual(config.with_suffix(".conf.bak").read_text(), "custom config")
        self.assertEqual(config.read_bytes(), self.expected)
        self.assertFalse(self.config.exists())

    def test_failed_backup_does_not_overwrite_original(self):
        self.config.parent.mkdir(parents=True)
        self.config.write_text("must survive")
        self.config.with_suffix(".conf.bak").mkdir()
        self.run_install(success=False)
        self.assertEqual(self.config.read_text(), "must survive")

    def test_backup_symlink_cannot_overwrite_source(self):
        self.config.parent.mkdir(parents=True)
        self.config.write_text("old config")
        backup = self.config.with_suffix(".conf.bak")
        backup.symlink_to(self.config)
        self.run_install()
        self.assertFalse(backup.is_symlink())
        self.assertEqual(backup.read_text(), "old config")
        self.assertEqual(self.config.read_bytes(), self.expected)


if __name__ == "__main__":
    unittest.main(verbosity=2)
