# Step-by-step: moving your email out of GoHighLevel

This file is the source for the client guide. Each step says what to do in plain words.

- 📸 marks a screenshot the capture agent must take (file name, what must be visible, what to highlight, what to blur).
- ➡️ `.env` marks a value that goes into the settings file.

**You don't have to follow this file yourself.** Start the coding agent in the `crm-marketing` folder and type `start` (Part 0). The agent walks you through Parts 1–5 one step at a time, using this file as its script, then does the technical work (Part 6) and tells you what to finish (Part 7). The steps take about 1–2 hours of your time, plus waiting for account approvals. This file is the reference for those steps and the source for the screenshot guide.

---

## Part 0: Get ready

**0.1 Download the kit.** Download (or clone) the `crm-marketing` repository and put the folder somewhere you can find it, e.g. your Documents folder.
📸 `00-01-kit-folder.png`: the folder open in Finder/Explorer.

**0.2 Install the coding agent.** Install **Claude Code** (<https://claude.com/claude-code>) or **Codex** (<https://openai.com/codex>) by following their install page.
> The agent may ask to install a few command-line tools it needs, such as the AWS CLI, the Hetzner CLI (`hcloud`) and `jq`. On a work laptop managed by your IT department, installing may need administrator rights: check beforehand that you have them, or ask IT.

**0.3 Start the agent in the folder.** Open a terminal in the `crm-marketing` folder:
- **Windows:** in Explorer, open the folder, click the address bar, type `cmd` (or `powershell`) and press Enter.
- **Mac:** right-click the folder in Finder and choose **New Terminal at Folder**. If you don't see it, turn it on under **System Settings → Keyboard → Keyboard Shortcuts → Services**.

Then type `claude` (or `codex`) and press Enter. The first time, you'll be asked to log in.
📸 `00-03-terminal.png`: the terminal in the folder with the agent started.

**0.4 Type `start`.** The agent takes it from here. It creates your settings file (`.env`), opens it in Notepad (Windows) or TextEdit (Mac), and guides you through Parts 1–5 one step at a time. Paste keys into that file, never into the chat: the chat is saved. You only type into the chat what the agent asks for, such as your company name.
📸 `00-04-agent-welcome.png`: the agent's welcome message and the opened `.env`.

**0.5 Get a password manager ready.** Use 1Password, Bitwarden or your browser's password manager. Every password and key you create goes there.

---

## Part 1: GoHighLevel (do this first)

> Your GoHighLevel sub-account is deleted on **{{SHUTDOWN_DATE}}**, and so is everything in it. Do this part now.

**1.1 Log in to your sub-account.** Go to <https://app.gohighlevel.com> and open **your business**, not the agency view.
📸 `01-01-subaccount-dashboard.png`: the sub-account dashboard. Highlight the business name at the top left.

**1.2 Export your contacts to a file (backup).**
1. Click **Contacts** in the left menu.
2. Tick the checkbox at the top of the list, then click **Select all _N_ contacts**.
3. Click **Export**, keep all fields selected, and confirm.
4. Download the file when it's ready and save it as `ghl-export/contacts-ui-export.csv` in the kit.
5. Write down the number of contacts.

📸 `01-02a-contacts-select-all.png`: the list with every contact selected. Highlight "Select all N contacts".
📸 `01-02b-export-dialog.png`: the export dialog. Highlight the Export button.
➡️ `.env`: `GHL_CONTACT_COUNT=` the number of contacts

**1.3 Find your Location ID.** Go to **Settings → Business Profile**. Copy the **Location ID** (it is also in the browser address bar after `/location/`).
📸 `01-03-location-id.png`: Business Profile with the Location ID field highlighted. Blur the ID.
➡️ `.env`: `GHL_LOCATION_ID=`

**1.4 Create a read-only API key.** This lets the agent download your contacts and data. It can't change anything.
1. Go to **Settings → Private Integrations → Create new Integration**.
2. Name it `migration-export`.
3. Tick **only** these read-only permissions:
   - `contacts.readonly`
   - `locations.readonly`
   - `locations/customFields.readonly`
   - `locations/tags.readonly`
   - `workflows.readonly`
   - `forms.readonly`
4. Click **Create**, then copy the token. **It is only shown once.**

📸 `01-04a-private-integrations-menu.png`: Settings with **Private Integrations** highlighted.
📸 `01-04b-integration-name.png`: the name field filled in.
📸 `01-04c-scopes.png`: the permissions list with the ticked ones highlighted. If the list needs scrolling, take several screenshots (`-c1`, `-c2`).
📸 `01-04d-token-shown.png`: the token screen with the copy button highlighted. Blur the token.
➡️ `.env`: `GHL_TOKEN=`
> Can't see "Private Integrations"? Your user needs to be an admin of the sub-account. Contact {{AGENCY}} before {{SHUTDOWN_DATE}}.

**1.5 Document each email workflow.** Workflows can't be exported, so you capture them yourself. The agent creates the folders and files for you and opens them in Notepad/TextEdit. For each workflow that sends emails:
1. Go to **Automation → Workflows** and open the workflow.
2. Take a screenshot of the whole flow. Zoom out so every step is visible.
3. Click each **Send Email** step, copy the subject and the email content, and paste them into a new file in the kit: `ghl-export/workflows/<workflow-name>/01-<short-name>.html`.
4. Copy `ghl-export/workflows/_template.md` into the same folder, rename it `workflow.md`, and fill it in: what starts the flow, the delays, the conditions, and what should happen to people who are in the middle of the flow.

📸 `01-05a-workflows-list.png`: the workflow list. Highlight one workflow.
📸 `01-05b-workflow-canvas.png`: an open workflow showing all its steps.
📸 `01-05c-email-step.png`: an open Send Email step. Highlight the subject and the content.
📸 `01-05d-workflow-md-filled.png`: a filled-in `workflow.md` in Notepad/TextEdit.

**1.6 List your forms and other GHL features.** Open `ghl-export/workflows/_overview.md` and fill in which forms on your website send sign-ups to GoHighLevel, and whether you use SMS, calendars or anything else. Those other features aren't covered by this solution.
📸 `01-06-overview-filled.png`: the filled-in overview in Notepad/TextEdit.

---

## Part 2: Amazon Web Services (AWS), which sends your emails

**2.1 Create an AWS account.**
1. Go to <https://aws.amazon.com> and click **Create an AWS account**.
2. Enter your email and an account name, e.g. your company name. Confirm the code sent to your email and choose a password (save it in your password manager).
3. If you're asked to pick a plan, choose the **Paid plan** (pay only for what you use). Sending emails costs cents. A free plan can limit services or expire.
4. Enter your contact details, payment card and phone verification.
5. Choose the **Basic support** plan (free).

📸 `02-01a-signup-start.png`: the sign-up page with **Create an AWS account** highlighted.
📸 `02-01b-plan-choice.png` (if shown): the plan choice with **Paid plan** highlighted.
📸 `02-01c-support-plan.png`: the support plans with **Basic** highlighted.
_The operator does the email, password, card and phone steps. No screenshots of those._

**2.2 Turn on two-step login (MFA).** Click your account name at the top right, choose **Security credentials**, then **Assign MFA device**, and follow the steps with an authenticator app.
📸 `02-02-assign-mfa.png`: Security credentials with **Assign MFA device** highlighted.

**2.3 (Recommended) Set a spending alert.** Search for **Budgets** at the top of the console, then choose **Create budget → Monthly cost budget**. Enter **$5** and your email.
📸 `02-03-budget.png`: the budget form with the amount and email highlighted.

**2.4 Choose Europe as your region.** At the top right, click the region name and choose **Europe (Frankfurt) eu-central-1**.
📸 `02-04-region.png`: the region menu with Frankfurt highlighted.
➡️ `.env`: `AWS_REGION=eu-central-1`

**2.5 Create a user for the agent.**
1. Search for **IAM** at the top and open it.
2. Click **Users → Create user**.
3. Name it `setup-agent`. **Do not** tick console access. Click **Next**.
4. Choose **Attach policies directly**.
5. Search for and tick **AmazonSESFullAccess**, then search for and tick **AmazonSNSFullAccess**.
6. Click **Next**, then **Create user**.

📸 `02-05a-iam-users.png`: IAM Users with **Create user** highlighted.
📸 `02-05b-user-name.png`: the name `setup-agent`, with the console access checkbox unticked (highlight it).
📸 `02-05c-policy-ses.png`: the search result with **AmazonSESFullAccess** ticked.
📸 `02-05d-policy-sns.png`: the search result with **AmazonSNSFullAccess** ticked.
📸 `02-05e-review.png`: the review page showing both policies.

**2.6 Allow the agent to create the sending user and the storage buckets.** This extra permission lets the agent create the sending user for Mailroom (`mailroom-sender`), give it its permissions and a key, and create two storage buckets and check their settings: one for the backups (`mailroom-backup-…`) and one where Amazon drops the emails people send to your domain before Mailroom picks them up (`mailroom-inbound-…`). It applies to that one user and those buckets only.

⚠️ **Treat the `setup-agent` key like a master key.** Because it may set the permissions of `mailroom-sender` and create keys for it, anyone holding the `setup-agent` key could give that user any permission in your AWS account. So keep the key only in your `.env`, never share it, and switch it off as soon as the setup is done (step 7.3). Mailroom itself never stores it: it runs with the `mailroom-sender` key only, and the agent hands the setup key to the server only for single setup commands, in a way that keeps it out of the server's logs.
1. Open the user `setup-agent` and choose **Permissions → Add permissions → Create inline policy**.
2. Click **JSON**, delete what's there, and paste in:

   ```json
   {
     "Version": "2012-10-17",
     "Statement": [
       {
         "Sid": "CreateMailroomSendingUser",
         "Effect": "Allow",
         "Action": ["iam:CreateUser", "iam:GetUser", "iam:TagUser",
                    "iam:PutUserPolicy", "iam:GetUserPolicy",
                    "iam:CreateAccessKey", "iam:ListAccessKeys", "iam:DeleteAccessKey"],
         "Resource": "arn:aws:iam::*:user/mailroom-sender"
       },
       {
         "Sid": "CreateMailroomBackupBucket",
         "Effect": "Allow",
         "Action": ["s3:CreateBucket", "s3:ListBucket",
                    "s3:GetBucketPublicAccessBlock", "s3:PutBucketPublicAccessBlock",
                    "s3:GetEncryptionConfiguration", "s3:PutEncryptionConfiguration",
                    "s3:GetLifecycleConfiguration", "s3:PutLifecycleConfiguration",
                    "s3:GetBucketVersioning", "s3:PutBucketVersioning"],
         "Resource": "arn:aws:s3:::mailroom-backup-*"
       },
       {
         "Sid": "CreateMailroomInboundBucket",
         "Effect": "Allow",
         "Action": ["s3:CreateBucket", "s3:ListBucket",
                    "s3:GetBucketPublicAccessBlock", "s3:PutBucketPublicAccessBlock",
                    "s3:GetEncryptionConfiguration", "s3:PutEncryptionConfiguration",
                    "s3:GetLifecycleConfiguration", "s3:PutLifecycleConfiguration",
                    "s3:GetBucketPolicy", "s3:PutBucketPolicy"],
         "Resource": "arn:aws:s3:::mailroom-inbound-*"
       }
     ]
   }
   ```
3. Click **Next**, name it `allow-mailroom-setup`, and click **Create policy**.

📸 `02-06a-add-inline.png`: the Add permissions menu with **Create inline policy** highlighted.
📸 `02-06b-json.png`: the JSON editor with the policy pasted in.
📸 `02-06c-permissions-done.png`: the user's permissions showing 3 policies.

**2.7 Create the access key.**
1. On the user `setup-agent`, open the **Security credentials** tab and click **Create access key**.
2. Choose **Command Line Interface (CLI)**, tick the confirmation box, and click **Next → Create access key**.
3. Copy the **Access key** and the **Secret access key**. **The secret is only shown once.**

📸 `02-07a-create-access-key.png`: the Security credentials tab with **Create access key** highlighted.
📸 `02-07b-use-case.png`: **Command Line Interface (CLI)** selected and the confirmation box ticked.
📸 `02-07c-keys-shown.png`: the keys screen with the copy buttons highlighted. Blur both keys.
➡️ `.env`: `AWS_ACCESS_KEY_ID=` and `AWS_SECRET_ACCESS_KEY=`

---

## Part 3: Hetzner, the server that runs your system

> **Already have a Coolify server?** Skip 3.1–3.3. Set `DEPLOY_TARGET=coolify` in `.env`. Either create an API token in Coolify (**Keys & Tokens → API tokens**, with write access) and put `COOLIFY_URL` and `COOLIFY_API_TOKEN` in `.env`, so the agent deploys for you, or leave them empty and follow [deploy/coolify.md](../deploy/coolify.md) yourself when the agent asks. Coolify must be able to read your copy of this repository (**Sources → GitHub App**). Coolify then handles HTTPS, and Mailroom shares the server with your other services.

> You don't create the server yourself. The agent creates it with the right size and settings using the key from step 3.3.

**3.1 Create a Hetzner account.** Go to <https://accounts.hetzner.com/signUp>, confirm your email, and enter your address and payment details. Hetzner may ask for ID verification, which can take a few hours.
📸 `03-01-signup.png`: the sign-up page.
_The operator does the personal details, payment and ID steps. No screenshots of those._

**3.2 Create a project.** Go to <https://console.hetzner.cloud>, click **+ New project**, name it `newsletter`, and open it.
📸 `03-02a-new-project.png`: the dialog with the name filled in.
📸 `03-02b-project-open.png`: the empty project.

**3.3 Create an API key.** In the project, click **Security → API tokens → Generate API token**. Name it `setup-agent`, choose **Read & Write**, and click **Generate**. Copy the token. **It is only shown once.**
📸 `03-03a-security-menu.png`: Security with the **API tokens** tab highlighted.
📸 `03-03b-generate.png`: the dialog with **Read & Write** highlighted.
📸 `03-03c-token-shown.png`: the token screen. Blur the token.
➡️ `.env`: `HCLOUD_TOKEN=`

**3.4 Sign the data processing agreement.** In your Hetzner account (<https://accounts.hetzner.com>), find **Data processing** / **Order processing agreement (AVV)**, and accept it.
📸 `03-04-avv.png`: the agreement page.

---

## Part 4: Your domain's DNS

The agent needs to add a few records to your domain: one web address for Mailroom (e.g. `newsletter.yourdomain.com`), and the records that prove to Gmail and Outlook that emails are really from you. Mailroom sends from **this one domain only**. **Pick option A or B.**

### Option A: Your domain uses Cloudflare (the agent does it for you)

**4A.1 Check that your domain is on Cloudflare.** Log in at <https://dash.cloudflare.com>. If your domain is listed as **Active**, choose this option.
📸 `04a-01-domain-active.png`: the account home with the domain and **Active** highlighted.

**4A.2 Create a limited API key.**
1. Click the profile icon at the top right, then **My Profile → API Tokens → Create Token**.
2. At the bottom, choose **Create Custom Token → Get started**.
3. Name it `setup-agent-dns`.
4. Under **Permissions**, add two rows:
   - `Zone` · `DNS` · `Edit`
   - `Zone` · `Zone` · `Read`
5. Under **Zone Resources**, choose `Include` · `Specific zone` · **your domain**.
6. Under **TTL**, set an end date about one month from today, so the key expires by itself.
7. Click **Continue to summary**, then **Create Token**. Copy the token. **It is only shown once.**

📸 `04a-02a-api-tokens.png`: API Tokens with **Create Token** highlighted.
📸 `04a-02b-custom-token.png`: **Create Custom Token** highlighted.
📸 `04a-02c-permissions.png`: **the key screenshot.** The two permission rows, the zone resource row with the domain, and the TTL, all highlighted.
📸 `04a-02d-summary.png`: the summary page showing DNS:Edit and Zone:Read for the one domain only.
📸 `04a-02e-token-shown.png`: the token screen. Blur the token.
➡️ `.env`: `DNS_MODE=cloudflare` and `CLOUDFLARE_API_TOKEN=`

With a key limited like this, the agent can only change DNS records for this one domain. It can't touch the rest of your Cloudflare account. It will still show you what it plans to add and ask before it does anything.

### Option B: Your domain is somewhere else (you add the records yourself)

**4B.1** Set `DNS_MODE=manual` in `.env`. If you don't know where your DNS is, that's fine. The agent checks it and tells you where to log in.
➡️ `.env`: `DNS_MODE=manual`

**4B.2** Later in Part 6, the agent stops and gives you a short table like this, and waits until you say it's done:

| Type | Name | Value | Priority |
|---|---|---|---|
| A | `newsletter.yourdomain.com` | `<your server's IPv4 address>` | |
| AAAA | `newsletter.yourdomain.com` | `<your server's IPv6 address>` (if it has one) | |
| CNAME | `xxxx._domainkey.yourdomain.com` (×3) | `xxxx.dkim.amazonses.com` | |
| MX | `bounce.yourdomain.com` | `feedback-smtp.eu-central-1.amazonses.com` | `10` |
| TXT | `bounce.yourdomain.com` | `v=spf1 include:amazonses.com ~all` | |
| TXT | `_dmarc.yourdomain.com` (only if you agreed to it) | `v=DMARC1; p=none; rua=mailto:you@yourdomain.com` (just `v=DMARC1; p=none` when your address isn't on your domain) | |

Log in to your DNS provider, find **DNS** / **DNS records** for your domain, and add each row. Many providers want only the part of the name before your domain (e.g. `newsletter`, not `newsletter.yourdomain.com`). Don't change or delete your existing records.
📸 `04b-02-record-table.png`: the agent's table in the terminal (captured during the Part 6 test run).

---

## Part 5: Your sender details

**5.1** Fill in the top of `.env`:
- `DOMAIN`: the one domain you send from, e.g. `yourdomain.com`
- `FROM_NAME`, `FROM_EMAIL`, `REPLY_TO_EMAIL`: who the emails come from, and where replies go. Both addresses must end in `@` + your `DOMAIN`
- `COMPANY_FOOTER`: company name, address and registration number (required at the bottom of marketing emails: without it Mailroom sends no newsletters or sequence emails)
- `OWNER_EMAIL`: your own inbox, used for test emails and alerts. Use an address on your `DOMAIN` if you can: until AWS approves your account (Part 6), test emails can only go to addresses on your own domain
- `TIMEZONE`: e.g. `Europe/Copenhagen`
- `APP_SUBDOMAIN`: the web address for Mailroom. The default `newsletter` gives `newsletter.yourdomain.com`
- `MAIL_FROM_SUBDOMAIN`: leave it as `bounce` unless `bounce.yourdomain.com` is already in use. Mail servers send their bounce reports there
- `GHL_SHUTDOWN_DATE`: {{SHUTDOWN_DATE}}

Save the file (**Ctrl+S** / **Cmd+S**).
📸 `05-01-env-complete.png`: the finished `.env` with all values blurred and the field names visible.

---

## Part 6: Let the agent do the work

When Parts 1–5 are done, the agent continues by itself in the same session. If you closed it, start it again in the folder (0.3) and type `start`: it reads its notes (`PROGRESS.md`) and continues where it stopped.

**6.1 Answer when it asks.** The agent works on its own, but stops at five points and waits for your "yes":
1. It shows what it exported from GoHighLevel. Check the numbers.
2. It shows the text for the AWS request to start sending. After that, watch your inbox, because AWS may email you questions.
3. DNS: either it shows what it will add in Cloudflare, or it gives you the table from 4B.2.
4. It shows how your contacts will be imported.
5. It shows the test results before any email goes to real people.

📸 `06-04-checkpoint-example.png`: an example of the agent stopping and asking for approval.

Expect it to take a couple of days in total, mostly waiting for AWS and DNS.

---

## Part 7: When the agent is done

**7.1 Save your Mailroom login.** The agent doesn't write your Mailroom password in the chat (the chat is saved). It opens the file `secrets/admin.txt` for you in Notepad or TextEdit instead. Copy the password from there into your password manager together with the address `https://newsletter.yourdomain.com`, then delete the file.
📸 `07-01-mailroom-login.png`: the Mailroom login page.

**7.1b Decide about the agent's API key.** The agent asks whether it should keep its Mailroom API key. Keep it if you want a coding agent to help you later (e.g. "write and send this month's newsletter"). Otherwise the agent deletes it. You can always create a new one in Mailroom under **API tokens**.

**7.2 Switch the forms on your website.** The agent gives you new form code or a new URL. Put it in place of the GoHighLevel forms (or ask your web agency to do it).

**7.3 Turn off the setup keys.** Do this on the day the agent finishes: the AWS setup key in particular can hand out any permission in your AWS account (step 2.6).
- **AWS:** IAM → Users → `setup-agent` → Security credentials → **Deactivate** on the access key, then **Delete** it. If you ever need the agent's help with AWS again (e.g. moving to a new server), create a new access key for that day and delete it afterwards; never switch an old key back on.
- **Cloudflare:** My Profile → API Tokens → `setup-agent-dns` → **Delete**.
- **Hetzner:** Security → API tokens → `setup-agent` → **Delete** (the server keeps running).

📸 `07-03a-aws-deactivate.png`, `07-03b-cf-delete.png`, `07-03c-hetzner-delete.png`: each button highlighted.

**7.4 After {{SHUTDOWN_DATE}}:** Delete the old GoHighLevel DNS records from the list the agent gives you.

**7.5 Keep the `ghl-export` folder safe.** It's your proof of where your contacts came from. Store it somewhere private, not on a shared drive.

Done. From now on, `OPERATIONS.md` tells you how to send a newsletter and change your sequences, either in Mailroom's web interface or by asking a coding agent to do it through Mailroom's API.
