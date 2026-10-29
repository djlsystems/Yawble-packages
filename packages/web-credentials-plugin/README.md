# Web Credentials 1.0.0

The `web-credentials-plugin` plugin: a plugin member that keeps a team's website sign-ins in one
table a person fills in - `password` (with an optional two-step code), `token`, or an `interactive`
sign-in the person does once in a browser the platform runs - checks each active entry on its own
schedule, and tells the team when one needs the person. A team member asks it for a short-lived
signed-in browser session for one entry, or has it open a page signed in and report what it shows
with a screenshot. No password, token or saved session is ever shown, returned, logged or written
to a document.

## Add it to a team

1. **Install it.** In **Marketplace → Browse**, choose **Get** on **Web Credentials** (plugin), then
   install it. It is installed once for the instance and hires nothing.
2. **Hire it.** On the team, **Add member → Web Credentials**. No settings are required at hire.
3. **Check on a schedule.** In the team's **Triggers** dialog, add a **schedule** trigger on the
   member, every 3600 seconds, instruction `verify all-due`, waking the Manager only if it hands
   back or fails.
4. **Add your sign-ins.** Open the member's **Settings** and add one entry per site login.

The solution `web-credentials` ships this plugin with a ready-made team and the hourly check, for
anyone who wants a team of its own. Its README covers the commands, methods, statuses and gaps; the
plugin skill `plugin-web-credentials-plugin` is the full reference a team's agents load.

## Build and test

```sh
./build.sh <out folder>   # needs node, npm and sh
npm ci && npm test        # fetches Google Chrome once into $TMPDIR/web-credentials-chrome
```
