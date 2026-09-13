# /// script
# dependencies = [
#     "playwright>=1.49.0",
#     "google-cloud-secret-manager>=2.20.0",
#     "google-auth>=2.30.0",
#     "requests>=2.31.0",
# ]
# ///
# ==============================================================================
# Instructions for AI agents:
# Do NOT execute this script autonomously.
# This script requires interactive human MFA login in a graphical browser on a local PC.
# If a session expires, notify the human user to run this script.
# ==============================================================================
import json
import os
import subprocess
import sys
import google.auth
from google.api_core import exceptions
from google.auth.exceptions import DefaultCredentialsError
from google.auth.transport.requests import Request
from google.cloud import secretmanager
from playwright.sync_api import sync_playwright

sys.dont_write_bytecode = True
SECRET_ID = os.environ.get("SECRET_ID", "mf-session-cookie")

def check_gcp_auth():
    print("Checking Google Cloud authentication...")
    try:
        credentials, default_project = google.auth.default()
    except DefaultCredentialsError:
        print("\nError: Google Cloud Application Default Credentials (ADC) not found.")
        print("Run the following command to log in:")
        print("  gcloud auth application-default login\n")
        sys.exit(1)

    try:
        if not credentials.valid:
            credentials.refresh(Request())
    except Exception as e:
        print(f"\nError: Google Cloud credentials expired or invalid: {e}")
        print("Run the following command to re-authenticate:")
        print("  gcloud auth application-default login\n")
        sys.exit(1)

    candidate_project = os.environ.get("GCP_PROJECT") or default_project
    if candidate_project:
        chosen = input(f"Target GCP Project ID [{candidate_project}]: ").strip()
        project_id = chosen if chosen else candidate_project
    else:
        project_id = input("Target GCP Project ID: ").strip()
        if not project_id:
            print("Error: GCP Project ID is required.")
            sys.exit(1)

    return credentials, project_id

def launch_browser(playwright):
    try:
        return playwright.chromium.launch(headless=False)
    except Exception:
        print("Playwright Chromium not found. Installing...")
        subprocess.run([sys.executable, "-m", "playwright", "install", "chromium"], check=True)
        return playwright.chromium.launch(headless=False)

def save_to_secret_manager(credentials, project_id: str, payload: str):
    client = secretmanager.SecretManagerServiceClient(credentials=credentials)
    parent = f"projects/{project_id}/secrets/{SECRET_ID}"
    try:
        response = client.add_secret_version(
            request={"parent": parent, "payload": {"data": payload.encode("utf-8")}}
        )
        print(f"Saved session to Secret Manager: {response.name}")
    except exceptions.NotFound:
        print(f"Error: Secret '{SECRET_ID}' not found in project '{project_id}'. Apply Terraform infrastructure first.")
        sys.exit(1)
    except exceptions.PermissionDenied:
        print(f"Error: Permission denied for secret '{SECRET_ID}'. Verify roles/secretmanager.secretVersionAdder.")
        sys.exit(1)
    except Exception as e:
        print(f"Error: Failed to save to Secret Manager: {e}")
        sys.exit(1)

def main():
    credentials, project_id = check_gcp_auth()
    print(f"Starting browser login for MoneyForward ME (Project: {project_id})...")

    with sync_playwright() as p:
        browser = launch_browser(p)
        context = browser.new_context()
        page = context.new_page()

        page.goto("https://moneyforward.com/sign_in")

        try:
            page.wait_for_url("https://moneyforward.com/", timeout=300000)
            print("Login completed successfully.")
        except Exception as e:
            print(f"Login timed out or failed: {e}")
            browser.close()
            sys.exit(1)

        storage_state = context.storage_state()
        browser.close()

    payload = json.dumps(storage_state, ensure_ascii=False)
    save_to_secret_manager(credentials, project_id, payload)
    print("Authentication setup finished successfully.")

if __name__ == "__main__":
    main()
