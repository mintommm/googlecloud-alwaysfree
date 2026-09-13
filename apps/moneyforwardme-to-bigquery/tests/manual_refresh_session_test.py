import sys
from unittest.mock import MagicMock, patch
import pytest
from google.auth.exceptions import DefaultCredentialsError
from google.api_core import exceptions

import manual_refresh_session as manual_refresh

class TestCheckGcpAuth:
    """Specification tests for ADC credentials validation and project ID prompt."""

    @patch("google.auth.default", side_effect=DefaultCredentialsError("No ADC found"))
    def test_fails_fast_when_credentials_not_found(self, mock_auth):
        with pytest.raises(SystemExit) as exc_info:
            manual_refresh.check_gcp_auth()
        assert exc_info.value.code == 1

    @patch("google.auth.default")
    @patch("builtins.input", return_value="")
    def test_accepts_default_project_when_user_presses_enter(self, mock_input, mock_auth):
        mock_creds = MagicMock()
        mock_creds.valid = True
        mock_auth.return_value = (mock_creds, "default-gcp-project")

        creds, project_id = manual_refresh.check_gcp_auth()

        assert project_id == "default-gcp-project"
        assert creds == mock_creds

    @patch("google.auth.default")
    @patch("builtins.input", return_value="custom-gcp-project")
    def test_uses_custom_project_when_specified(self, mock_input, mock_auth):
        mock_creds = MagicMock()
        mock_creds.valid = True
        mock_auth.return_value = (mock_creds, "default-gcp-project")

        creds, project_id = manual_refresh.check_gcp_auth()

        assert project_id == "custom-gcp-project"

class TestSaveToSecretManager:
    """Specification tests for Secret Manager storage and error handling."""

    @patch("google.cloud.secretmanager.SecretManagerServiceClient")
    def test_adds_secret_version_successfully(self, mock_client_cls):
        mock_client = MagicMock()
        mock_client_cls.return_value = mock_client
        mock_response = MagicMock()
        mock_response.name = "projects/my-p/secrets/mf-session-cookie/versions/1"
        mock_client.add_secret_version.return_value = mock_response

        mock_creds = MagicMock()
        manual_refresh.save_to_secret_manager(mock_creds, "my-p", '{"test": "data"}')

        mock_client.add_secret_version.assert_called_once_with(
            request={
                "parent": "projects/my-p/secrets/mf-session-cookie",
                "payload": {"data": b'{"test": "data"}'},
            }
        )

    @patch("google.cloud.secretmanager.SecretManagerServiceClient")
    def test_exits_with_error_on_not_found(self, mock_client_cls):
        mock_client = MagicMock()
        mock_client_cls.return_value = mock_client
        mock_client.add_secret_version.side_effect = exceptions.NotFound("Secret not found")

        mock_creds = MagicMock()
        with pytest.raises(SystemExit) as exc_info:
            manual_refresh.save_to_secret_manager(mock_creds, "my-p", "payload")

        assert exc_info.value.code == 1

    @patch("google.cloud.secretmanager.SecretManagerServiceClient")
    def test_exits_with_error_on_permission_denied(self, mock_client_cls):
        mock_client = MagicMock()
        mock_client_cls.return_value = mock_client
        mock_client.add_secret_version.side_effect = exceptions.PermissionDenied("Access denied")

        mock_creds = MagicMock()
        with pytest.raises(SystemExit) as exc_info:
            manual_refresh.save_to_secret_manager(mock_creds, "my-p", "payload")

        assert exc_info.value.code == 1

class TestLaunchBrowser:
    """Specification tests for browser launch and fallback auto-installation."""

    def test_launches_chromium_without_reinstall_when_installed(self, mocker):
        mock_playwright = mocker.MagicMock()
        mock_browser = mocker.MagicMock()
        mock_playwright.chromium.launch.return_value = mock_browser

        browser = manual_refresh.launch_browser(mock_playwright)

        assert browser == mock_browser
        mock_playwright.chromium.launch.assert_called_once_with(headless=False)

    def test_auto_installs_chromium_and_retries_on_launch_error(self, mocker):
        mock_playwright = mocker.MagicMock()
        mock_browser = mocker.MagicMock()
        mock_playwright.chromium.launch.side_effect = [RuntimeError("Not installed"), mock_browser]

        mock_subprocess = mocker.patch("subprocess.run")

        browser = manual_refresh.launch_browser(mock_playwright)

        assert browser == mock_browser
        assert mock_playwright.chromium.launch.call_count == 2
        mock_subprocess.assert_called_once()

class TestMainFlow:
    """Specification tests for the end-to-end interactive authentication orchestration."""

    @patch("manual_refresh_session.save_to_secret_manager")
    @patch("manual_refresh_session.launch_browser")
    @patch("manual_refresh_session.sync_playwright")
    @patch("manual_refresh_session.check_gcp_auth")
    def test_main_executes_login_flow_and_saves_secret_successfully(
        self, mock_auth, mock_playwright, mock_launch, mock_save
    ):
        mock_creds = MagicMock()
        mock_auth.return_value = (mock_creds, "target-project")

        mock_browser = MagicMock()
        mock_launch.return_value = mock_browser
        mock_context = MagicMock()
        mock_context.storage_state.return_value = {"cookies": ["session_data"]}
        mock_browser.new_context.return_value = mock_context
        mock_page = MagicMock()
        mock_context.new_page.return_value = mock_page

        manual_refresh.main()

        mock_page.goto.assert_called_once_with("https://moneyforward.com/sign_in")
        mock_page.wait_for_url.assert_called_once_with("https://moneyforward.com/", timeout=300000)
        mock_save.assert_called_once_with(mock_creds, "target-project", '{"cookies": ["session_data"]}')
        mock_browser.close.assert_called_once()

    @patch("manual_refresh_session.launch_browser")
    @patch("manual_refresh_session.sync_playwright")
    @patch("manual_refresh_session.check_gcp_auth")
    def test_main_exits_and_closes_browser_on_login_timeout(
        self, mock_auth, mock_playwright, mock_launch
    ):
        mock_auth.return_value = (MagicMock(), "target-project")

        mock_browser = MagicMock()
        mock_launch.return_value = mock_browser
        mock_context = MagicMock()
        mock_browser.new_context.return_value = mock_context
        mock_page = MagicMock()
        mock_page.wait_for_url.side_effect = RuntimeError("Timeout waiting for login")
        mock_context.new_page.return_value = mock_page

        with pytest.raises(SystemExit) as exc_info:
            manual_refresh.main()

        assert exc_info.value.code == 1
        mock_browser.close.assert_called_once()

