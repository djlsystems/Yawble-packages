# Job Tracker

A solution package: a team that finds job postings, tracks them on a page, drafts a cover letter
when you press **Apply**, and reviews the drafts each week. It is version 1.1.0 of the worked
example in Yawble's `docs/solutions.md` (its `samples/solutions/job-tracker` with the 1.1.0 overlay
applied).

| Part | What it is |
|---|---|
| `solution.json` | The team: four members, five triggers, two skills, one site, what you provide. |
| `plugins/job-board/` | A stand-in job board plugin, 0.2.0 (Python, no network): reads `postings.json`, writes matches to the tracker and publishes `plugin.job-board.posting-found`. Its `build.sh` lays out the installable version. |
| `skills/job-search-playbook.md` | The team's own skill, offered to its Manager and members only. |
| `skills/interview-prep.md` | How to prepare the person's notes when a posting reaches an interview. |
| `sites/tracker/` | The page: the postings, with **Apply** and **Not for me**. |
| `tools/make-cover-letter.py` | The first-draft generator the Writer runs as `{solution}/make-cover-letter.py`. |

## What you are asked for

- **Resume** (required): your reference resume, `.docx` or PDF, uploaded to the team's `Resume` folder.
- **The Scout's sources** (optional): which boards it may read. Until you tick `sample`, it reads none.

## The page fills right after install

"Scan for postings" sets `runAtInstall`, so the install runs it once as soon as its last step
succeeds, and then every hour as usual. With a source ticked, the tracker page fills right after
install instead of an hour later; the install's result says "Scan for postings ran now".

## Bounded settings

The Scout's number settings declare their bounds in `plugin.json`, so a stray keystroke cannot make
it skip every posting (a `salaryMax` of -2 would):

| Setting | Bounds | What it does |
|---|---|---|
| `salaryMin`, `salaryMax` | `min: 0` | skips a posting whose annual range is wholly outside them |
| `hourlyMin`, `hourlyMax` | `min: 0` | the same for an hourly rate |
| `archiveAfterDays` | `min: 1`, `integer: true` | skips a posting posted more than that many days ago |

All are optional, set in the Scout's settings. The Host refuses a value outside its bounds from
every writer - the settings form, Add member, and a solution install or update - naming the field
and the bound, and writes nothing.

## Secrets

The install binds the Scout's five secrets by key name; you do not bind them by hand. Each is
needed only when you tick its source, and the install shows which the Host already has set.

| Key | For |
|---|---|
| `ADZUNA_APP_ID`, `ADZUNA_APP_KEY` | `adzuna` |
| `USAJOBS_API_KEY`, `USAJOBS_USER_AGENT` | `usajobs` |
| `THEMUSE_API_KEY` | `themuse` |

To set one, the operator runs `yawble secret set ADZUNA_APP_ID` (it prompts for the value) and then
`yawble up` to restart the Host. A key left unset is not an error: that source fails until it is
set. This sample's board is a stand-in and calls none of them; it says which ticked board has no keys.

## The team

- **Coordinator** (Manager) summarises the tracker each weekday at 08:00 London time.
- **Scout** (the `job-board` plugin) scans every hour, once right at install.
- **Writer** notes how well your resume fits each new posting, and drafts a letter when you press
  **Apply**.
- **Reviewer** adds three review notes to each draft in `Drafts/`, every Monday at 09:00.

## Check it

From the repository root, build it and check the zip's content with Yawble's own check:

```
scripts/build.sh catalog-2026.10.09.1
mkdir /tmp/job-tracker && cd /tmp/job-tracker && unzip <repository>/dist/job-tracker-1.1.0.zip
yawble solution check /tmp/job-tracker
```
