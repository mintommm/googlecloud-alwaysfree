import io
import json
import os
from datetime import date
from unittest.mock import MagicMock, patch
import pytest

# Dummy environment variables must be populated before importing sync module to satisfy Fail-Fast checks
os.environ.setdefault("PROJECT_ID", "test-project-id")
os.environ.setdefault("SECRET_ID", "mf-session-cookie")
os.environ.setdefault("DATASET_ID", "moneyforward")
os.environ.setdefault("TABLE_ID", "raw_transactions")
os.environ.setdefault("DISCORD_WEBHOOK_URL", "https://discord.com/api/webhooks/test/dummy")

import requests

from sync import (
    parse_mf_csv,
    download_monthly_csv,
    merge_transactions,
    run_sync,
    parse_payload,
    notify_discord,
    get_secret,
    update_secret,
    SessionExpiredError,
    SyncHandler,
)

SAMPLE_CSV_HEADER = "計算対象,日付,内容,金額（円）,保有金融機関,大項目,中項目,メモ,振替,ID\n"
SAMPLE_CSV_ROW_1 = "1,2024/05/15,セブン-イレブン,500,三井住友カード,食費,食料品,昼食,0,tx_12345\n"
SAMPLE_CSV_ROW_2 = "0,2024/05/16,給与振込,300000,みずほ銀行,収入,給与,,0,tx_12346\n"

class TestParseMfCsv:
    """Specification tests for MoneyForward CSV parsing and row hash generation."""

    def test_parses_valid_csv_and_normalizes_data_types(self):
        csv_text = SAMPLE_CSV_HEADER + SAMPLE_CSV_ROW_1 + SAMPLE_CSV_ROW_2
        csv_bytes = csv_text.encode("cp932")

        rows = parse_mf_csv(csv_bytes)

        assert len(rows) == 2

        assert rows[0]["is_calculation_target"] is True
        assert rows[0]["transaction_date"] == "2024/05/15"
        assert rows[0]["content"] == "セブン-イレブン"
        assert rows[0]["amount"] == 500
        assert rows[0]["financial_institution"] == "三井住友カード"
        assert rows[0]["large_category"] == "食費"
        assert rows[0]["middle_category"] == "食料品"
        assert rows[0]["memo"] == "昼食"
        assert rows[0]["transfer_flag"] is False
        assert rows[0]["transaction_id"] == "tx_12345"
        assert len(rows[0]["row_hash"]) == 64

        assert rows[1]["is_calculation_target"] is False
        assert rows[1]["amount"] == 300000
        assert rows[1]["memo"] == ""

    def test_generates_identical_row_hash_for_same_record(self):
        csv_text = SAMPLE_CSV_HEADER + SAMPLE_CSV_ROW_1
        rows1 = parse_mf_csv(csv_text.encode("cp932"))
        rows2 = parse_mf_csv(csv_text.encode("cp932"))

        assert rows1[0]["row_hash"] == rows2[0]["row_hash"]

    def test_generates_different_row_hash_when_any_column_changes(self):
        modified_row = "1,2024/05/15,セブン-イレブン,550,三井住友カード,食費,食料品,昼食,0,tx_12345\n"
        rows_original = parse_mf_csv((SAMPLE_CSV_HEADER + SAMPLE_CSV_ROW_1).encode("cp932"))
        rows_modified = parse_mf_csv((SAMPLE_CSV_HEADER + modified_row).encode("cp932"))

        assert rows_original[0]["row_hash"] != rows_modified[0]["row_hash"]

    def test_skips_header_empty_lines_and_malformed_rows(self):
        csv_text = (
            SAMPLE_CSV_HEADER
            + "\n"
            + "1,2024/05/15,Short row only 3 cols\n"
            + SAMPLE_CSV_ROW_1
            + "   \n"
        )
        rows = parse_mf_csv(csv_text.encode("cp932"))

        assert len(rows) == 1
        assert rows[0]["transaction_id"] == "tx_12345"

    def test_handles_cp932_specific_japanese_characters_without_corruption(self):
        special_row = "1,2024/05/15,㈱髙島屋～テスト,1000,銀行,買い物,百貨店,,0,tx_99999\n"
        csv_text = SAMPLE_CSV_HEADER + special_row
        rows = parse_mf_csv(csv_text.encode("cp932"))

        assert len(rows) == 1
        # CP932 decodes 0x8160 as fullwidth tilde (U+FF5E) and preserves NEC/IBM extensions like 髙 and ㈱
        assert rows[0]["content"] == "㈱髙島屋～テスト"

class TestDownloadMonthlyCsv:
    """Specification tests for monthly CSV downloads and session expiry detection."""

    def test_downloads_csv_successfully_when_session_valid(self, mocker):
        mock_page = mocker.MagicMock()
        mock_resp = mocker.MagicMock()
        mock_resp.url = "https://moneyforward.com/cf/csv?from=2024%2F05%2F01&month=05&year=2024"
        mock_page.goto.return_value = mock_resp

        mock_download = mocker.MagicMock()
        mock_download.path.return_value = "/tmp/dummy.csv"
        mock_download_context = mocker.MagicMock()
        mock_download_context.value = mock_download

        mock_page.expect_download.return_value.__enter__.return_value = mock_download_context

        mocker.patch("builtins.open", mocker.mock_open(read_data=b"dummy_csv_content"))

        result = download_monthly_csv(mock_page, "2024", "05")

        assert result == b"dummy_csv_content"
        mock_page.goto.assert_called_once_with(
            "https://moneyforward.com/cf/csv?from=2024%2F05%2F01&month=05&year=2024"
        )

    def test_raises_session_expired_error_when_redirected_to_sign_in(self, mocker):
        mock_page = mocker.MagicMock()
        mock_resp = mocker.MagicMock()
        mock_resp.url = "https://moneyforward.com/sign_in"
        mock_page.goto.return_value = mock_resp
        mock_page.expect_download.return_value.__enter__.return_value = mocker.MagicMock()

        with pytest.raises(SessionExpiredError) as exc_info:
            download_monthly_csv(mock_page, "2024", "05")

        assert "Session expired" in str(exc_info.value)

class TestMergeTransactions:
    """Specification tests for BigQuery staging load, MERGE statement, and cleanup."""

    def test_returns_zero_and_skips_bigquery_when_rows_empty(self, mocker):
        mock_bq = mocker.MagicMock()
        inserted = merge_transactions(mock_bq, [])

        assert inserted == 0
        mock_bq.load_table_from_json.assert_not_called()

    def test_executes_load_and_merge_query_and_deletes_staging_table(self, mocker):
        mock_bq = mocker.MagicMock()
        mock_load_job = mocker.MagicMock()
        mock_bq.load_table_from_json.return_value = mock_load_job

        mock_query_job = mocker.MagicMock()
        mock_query_job.num_dml_affected_rows = 5
        mock_bq.query.return_value = mock_query_job

        rows = [{"row_hash": "hash1", "transaction_date": "2024-05-01"}]
        inserted = merge_transactions(mock_bq, rows)

        assert inserted == 5
        mock_bq.load_table_from_json.assert_called_once()
        mock_load_job.result.assert_called_once()

        query_arg = mock_bq.query.call_args[0][0]
        assert "MERGE `test-project-id.moneyforward.raw_transactions`" in query_arg
        assert "ON T.transaction_date = S.transaction_date AND T.row_hash = S.row_hash" in query_arg
        assert "WHEN NOT MATCHED THEN" in query_arg
        mock_bq.delete_table.assert_called_once()

    def test_ensures_staging_table_cleanup_even_if_merge_fails(self, mocker):
        mock_bq = mocker.MagicMock()
        mock_bq.load_table_from_json.return_value = mocker.MagicMock()
        mock_bq.query.side_effect = RuntimeError("BigQuery query error")

        rows = [{"row_hash": "hash1", "transaction_date": "2024-05-01"}]
        with pytest.raises(RuntimeError):
            merge_transactions(mock_bq, rows)

        mock_bq.delete_table.assert_called_once()

class TestRunSync:
    """Specification tests for sync orchestration, sleep intervals, and secret updates."""

    @patch("sync.merge_transactions", return_value=3)
    @patch("sync.download_monthly_csv", return_value=b"col1\ncol2\n")
    @patch("sync.parse_mf_csv", return_value=[{"row_hash": "h1"}, {"row_hash": "h2"}, {"row_hash": "h3"}])
    @patch("sync.update_secret")
    @patch("sync.get_secret", return_value='{"cookies": []}')
    @patch("sync.notify_discord")
    @patch("sync.sync_playwright")
    @patch("sync.time.sleep")
    def test_default_target_months_fetches_three_months_and_sleeps(
        self, mock_sleep, mock_playwright, mock_discord, mock_get_secret,
        mock_update_secret, mock_parse, mock_download, mock_merge
    ):
        mock_browser = MagicMock()
        mock_context = MagicMock()
        mock_context.storage_state.return_value = {"cookies": ["new"]}
        mock_browser.new_context.return_value = mock_context
        mock_playwright.return_value.__enter__.return_value.chromium.launch.return_value = mock_browser

        result = run_sync(None)

        assert result["status"] == "success"
        assert result["inserted_count"] == 3
        assert mock_download.call_count == 3
        assert mock_sleep.call_count == 2
        mock_sleep.assert_called_with(3)
        mock_update_secret.assert_called_once()
        mock_discord.assert_called_once()

class TestParsePayload:
    """Specification tests for HTTP request payload parsing."""

    def test_returns_none_for_empty_body(self):
        assert parse_payload(b"") is None
        assert parse_payload(b"{}") is None

    def test_parses_single_month_payload(self):
        months = parse_payload(b'{"month": "2024-05"}')
        assert months == [date(2024, 5, 1)]

    def test_parses_date_range_payload(self):
        months = parse_payload(b'{"from": "2024-01", "to": "2024-03"}')
        assert months == [date(2024, 1, 1), date(2024, 2, 1), date(2024, 3, 1)]

class TestSyncHandler:
    """Specification tests for HTTP POST handler responses and status codes."""

    @patch("sync.run_sync", return_value={"status": "success", "inserted_count": 10})
    def test_returns_http_200_on_success(self, mock_run_sync):
        handler = MagicMock(spec=SyncHandler)
        handler.headers = {"Content-Length": "0"}
        handler.rfile = io.BytesIO(b"")
        handler.wfile = io.BytesIO()

        SyncHandler.do_POST(handler)

        handler.send_response.assert_called_once_with(200)
        resp_json = json.loads(handler.wfile.getvalue().decode("utf-8"))
        assert resp_json["status"] == "success"

    @patch("sync.run_sync", side_effect=SessionExpiredError("Session expired message"))
    @patch("sync.notify_discord")
    def test_returns_http_401_on_session_expired(self, mock_notify, mock_run_sync):
        handler = MagicMock(spec=SyncHandler)
        handler.headers = {"Content-Length": "0"}
        handler.rfile = io.BytesIO(b"")
        handler.wfile = io.BytesIO()

        SyncHandler.do_POST(handler)

        handler.send_response.assert_called_once_with(401)
        mock_notify.assert_called_once_with("Session expired message")
        resp_json = json.loads(handler.wfile.getvalue().decode("utf-8"))
        assert resp_json["error"] == "session_expired"

    @patch("sync.run_sync", side_effect=RuntimeError("Database failure"))
    @patch("sync.notify_discord")
    def test_returns_http_500_on_internal_error(self, mock_notify, mock_run_sync):
        handler = MagicMock(spec=SyncHandler)
        handler.headers = {"Content-Length": "0"}
        handler.rfile = io.BytesIO(b"")
        handler.wfile = io.BytesIO()

        SyncHandler.do_POST(handler)

        handler.send_response.assert_called_once_with(500)
        mock_notify.assert_called_once()
        resp_json = json.loads(handler.wfile.getvalue().decode("utf-8"))
        assert "Database failure" in resp_json["error"]

class TestNotifyDiscord:
    """Specification tests for Discord webhook notifications."""

    @patch("sync.requests.post")
    def test_sends_post_request_when_webhook_url_present(self, mock_post, monkeypatch):
        monkeypatch.setattr("sync.DISCORD_WEBHOOK_URL", "https://discord.com/api/webhooks/mock")
        notify_discord("Test message")
        mock_post.assert_called_once_with(
            "https://discord.com/api/webhooks/mock",
            json={"content": "Test message"},
            timeout=10,
        )

    @patch("sync.requests.post")
    def test_skips_silently_when_webhook_url_empty(self, mock_post, monkeypatch):
        monkeypatch.setattr("sync.DISCORD_WEBHOOK_URL", "")
        notify_discord("Test message")
        mock_post.assert_not_called()

    @patch("sync.requests.post", side_effect=requests.RequestException("Network timeout"))
    def test_suppresses_and_logs_network_exceptions(self, mock_post, monkeypatch):
        monkeypatch.setattr("sync.DISCORD_WEBHOOK_URL", "https://discord.com/api/webhooks/mock")
        notify_discord("Test message")
        mock_post.assert_called_once()

class TestSecretManagerOperations:
    """Specification tests for Secret Manager retrieval and update."""

    @patch("sync.secretmanager.SecretManagerServiceClient")
    def test_get_secret_returns_decoded_payload(self, mock_client_cls):
        mock_client = MagicMock()
        mock_client_cls.return_value = mock_client
        mock_resp = MagicMock()
        mock_resp.payload.data = b'{"cookies": ["test"]}'
        mock_client.access_secret_version.return_value = mock_resp

        result = get_secret("my-secret")
        assert result == '{"cookies": ["test"]}'
        mock_client.access_secret_version.assert_called_once_with(
            request={"name": "projects/test-project-id/secrets/my-secret/versions/latest"}
        )

    @patch("sync.secretmanager.SecretManagerServiceClient")
    def test_update_secret_adds_version_with_utf8_bytes(self, mock_client_cls):
        mock_client = MagicMock()
        mock_client_cls.return_value = mock_client
        mock_resp = MagicMock()
        mock_resp.name = "projects/test-project-id/secrets/my-secret/versions/2"
        mock_client.add_secret_version.return_value = mock_resp

        update_secret("my-secret", '{"new": "data"}')
        mock_client.add_secret_version.assert_called_once_with(
            request={
                "parent": "projects/test-project-id/secrets/my-secret",
                "payload": {"data": b'{"new": "data"}'},
            }
        )

class TestPayloadAndMonthCalculations:
    """Specification tests for payload parsing edge cases and date calculations."""

    def test_parses_year_boundary_date_range(self):
        months = parse_payload(b'{"from": "2024-11", "to": "2025-02"}')
        assert months == [
            date(2024, 11, 1),
            date(2024, 12, 1),
            date(2025, 1, 1),
            date(2025, 2, 1),
        ]

    def test_returns_empty_list_when_from_date_is_after_to_date(self):
        months = parse_payload(b'{"from": "2024-05", "to": "2024-01"}')
        assert months == []

