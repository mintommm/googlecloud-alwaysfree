# /// script
# dependencies = [
#     "playwright",
#     "google-cloud-secret-manager",
#     "google-cloud-bigquery",
#     "python-dateutil",
#     "requests",
# ]
# ///
import csv
import hashlib
import io
import json
import logging
import os
import sys
import time
from datetime import date, datetime
from http.server import HTTPServer, BaseHTTPRequestHandler
from dateutil.relativedelta import relativedelta
from google.cloud import secretmanager, bigquery
from playwright.sync_api import sync_playwright
import requests

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")

PROJECT_ID = os.environ["PROJECT_ID"]
SECRET_ID = os.environ["SECRET_ID"]
DATASET_ID = os.environ["DATASET_ID"]
TABLE_ID = os.environ["TABLE_ID"]
DISCORD_WEBHOOK_URL = os.environ.get("DISCORD_WEBHOOK_URL", "")

class SessionExpiredError(Exception):
    pass

def get_secret(secret_id: str) -> str:
    client = secretmanager.SecretManagerServiceClient()
    name = f"projects/{PROJECT_ID}/secrets/{secret_id}/versions/latest"
    resp = client.access_secret_version(request={"name": name})
    return resp.payload.data.decode("utf-8")

def update_secret(secret_id: str, payload: str):
    client = secretmanager.SecretManagerServiceClient()
    parent = f"projects/{PROJECT_ID}/secrets/{secret_id}"
    resp = client.add_secret_version(
        request={"parent": parent, "payload": {"data": payload.encode("utf-8")}}
    )
    logging.info(f"Updated Secret Manager secret '{secret_id}' with new session: {resp.name}")

def notify_discord(message: str):
    if not DISCORD_WEBHOOK_URL:
        return
    try:
        requests.post(DISCORD_WEBHOOK_URL.strip(), json={"content": message}, timeout=10)
    except Exception as e:
        logging.error(f"Failed to send Discord notification: {e}")

def parse_mf_csv(csv_bytes: bytes) -> list[dict]:
    text = csv_bytes.decode("cp932", errors="replace")
    reader = csv.reader(io.StringIO(text))
    header = next(reader, None)
    rows = []
    for row in reader:
        if not row or len(row) < 10:
            continue
        row_str = "|".join(row[:10])
        row_hash = hashlib.sha256(row_str.encode("utf-8")).hexdigest()
        rows.append({
            "is_calculation_target": row[0] == "1",
            "transaction_date": row[1],
            "content": row[2],
            "amount": int(row[3]) if row[3] else 0,
            "financial_institution": row[4],
            "large_category": row[5],
            "middle_category": row[6],
            "memo": row[7],
            "transfer_flag": row[8] == "1",
            "transaction_id": row[9],
            "row_hash": row_hash,
        })
    return rows

def download_monthly_csv(page, year: str, month: str) -> bytes:
    url = f"https://moneyforward.com/cf/csv?from={year}%2F{month}%2F01&month={month}&year={year}"
    logging.info(f"Downloading CSV for {year}-{month}: {url}")
    with page.expect_download(timeout=30000) as download_info:
        resp = page.goto(url)
        # MoneyForward redirects to sign_in (HTTP 200/302) instead of returning 401/403 when session expires
        if resp and "sign_in" in resp.url:
            raise SessionExpiredError("[moneyforwardme-to-bigquery] Session expired. Requires human re-authentication via manual_refresh_session.py.")
    download = download_info.value
    path = download.path()
    with open(path, "rb") as f:
        return f.read()

def merge_transactions(bq_client: bigquery.Client, rows: list[dict]) -> int:
    if not rows:
        return 0
    staging_table_id = f"{TABLE_ID}_staging_{int(time.time())}"
    staging_table_ref = f"{PROJECT_ID}.{DATASET_ID}.{staging_table_id}"
    table_ref = f"{PROJECT_ID}.{DATASET_ID}.{TABLE_ID}"

    try:
        load_job = bq_client.load_table_from_json(
            rows,
            staging_table_ref,
            job_config=bigquery.LoadJobConfig(autodetect=True),
        )
        load_job.result()

        merge_sql = f"""
        MERGE `{table_ref}` AS T
        USING `{staging_table_ref}` AS S
        ON T.transaction_date = S.transaction_date AND T.row_hash = S.row_hash
        WHEN NOT MATCHED THEN
          INSERT (
            is_calculation_target, transaction_date, content, amount,
            financial_institution, large_category, middle_category, memo,
            transfer_flag, transaction_id, row_hash
          )
          VALUES (
            S.is_calculation_target, S.transaction_date, S.content, S.amount,
            S.financial_institution, S.large_category, S.middle_category, S.memo,
            S.transfer_flag, S.transaction_id, S.row_hash
          )
        """
        query_job = bq_client.query(merge_sql)
        query_job.result()
        inserted_count = query_job.num_dml_affected_rows or 0
        logging.info(f"Appended {inserted_count} rows into BigQuery (inspected {len(rows)} total).")
        return inserted_count
    finally:
        bq_client.delete_table(staging_table_ref, not_found_ok=True)

def run_sync(target_months: list[date] | None = None) -> dict:
    logging.info("Starting sync job...")
    session_json = get_secret(SECRET_ID)
    storage_state = json.loads(session_json)

    if not target_months:
        today = date.today()
        # Credit card settlements and bank syncs are delayed or retrospectively edited for up to 2 months
        target_months = [today, today - relativedelta(months=1), today - relativedelta(months=2)]

    bq_client = bigquery.Client(project=PROJECT_ID)
    all_rows = []

    with sync_playwright() as p:
        browser = p.chromium.launch(headless=True)
        context = browser.new_context(storage_state=storage_state)
        page = context.new_page()

        try:
            for idx, target in enumerate(target_months):
                if idx > 0:
                    # MoneyForward terminates active sessions (302 redirect) if CSVs are requested too rapidly
                    time.sleep(3)

                year = target.strftime("%Y")
                month = target.strftime("%m")
                csv_bytes = download_monthly_csv(page, year, month)
                all_rows.extend(parse_mf_csv(csv_bytes))

            new_storage_state = context.storage_state()
        finally:
            browser.close()

    # MoneyForward extends session validity on each access; persisting latest state maintains autonomous loop
    update_secret(SECRET_ID, json.dumps(new_storage_state, ensure_ascii=False))

    inserted_count = merge_transactions(bq_client, all_rows)
    msg = f"[moneyforwardme-to-bigquery] Sync completed: {inserted_count} rows inserted ({len(all_rows) - inserted_count} skipped duplicates, session extended)."
    notify_discord(msg)
    return {"status": "success", "inserted_count": inserted_count, "total_inspected": len(all_rows)}

def parse_payload(body: bytes) -> list[date] | None:
    if not body:
        return None
    data = json.loads(body.decode("utf-8"))
    if "month" in data and data["month"]:
        return [datetime.strptime(data["month"], "%Y-%m").date()]
    if "from" in data and "to" in data:
        cur = datetime.strptime(data["from"], "%Y-%m").date()
        end = datetime.strptime(data["to"], "%Y-%m").date()
        months = []
        while cur <= end:
            months.append(cur)
            cur += relativedelta(months=1)
        return months
    return None

class SyncHandler(BaseHTTPRequestHandler):
    def do_POST(self):
        try:
            content_length = int(self.headers.get("Content-Length", 0))
            body = self.rfile.read(content_length) if content_length > 0 else b""
            target_months = parse_payload(body)

            result = run_sync(target_months)
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps(result).encode("utf-8"))
        except SessionExpiredError as e:
            notify_discord(str(e))
            self.send_response(401)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps({"status": "error", "error": "session_expired"}).encode("utf-8"))
        except Exception as e:
            logging.exception("Unhandled exception during sync")
            notify_discord(f"[moneyforwardme-to-bigquery] Sync failed: {e}")
            self.send_response(500)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps({"status": "error", "error": str(e)}).encode("utf-8"))

def run_server():
    port = int(os.environ.get("PORT", 8080))
    server = HTTPServer(("0.0.0.0", port), SyncHandler)
    logging.info(f"Server started on port {port}")
    server.serve_forever()

if __name__ == "__main__":
    run_server()
