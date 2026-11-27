# Linux login traces: where sessions leave records

[Back to diagnostic tools](../tools.md)

Research notes behind `list_user_sessions` and `get_login_history`. Every claim
was checked against primary sources: official release notes, upstream man pages
and source code, and actual Debian package payloads from deb.debian.org
(package internals inspected in-memory from official `.deb`/`.debian.tar.xz`
files). Verified 2026-11-27. Facts that could not be verified are listed at the
end; do not treat them as established.

## Debian 13 removed last, lastb and lastlog

- Debian trixie release notes §5.1.9: the **util-linux** package no longer
  provides `last` or `lastb`, and the **login** package no longer provides
  `lastlog`. Reason: Y2038 (32-bit time_t in utmp/wtmp/lastlog formats).
  Replacements: `wtmpdb` (+`libpam-wtmpdb`) and `lastlog2` (+`libpam-lastlog2`);
  `lslogins` (and `lslogins --failed` ≈ lastb) remain in util-linux.
  <https://www.debian.org/releases/trixie/release-notes/issues.html#the-last-lastb-and-lastlog-commands-have-been-replaced>
- This is a Debian packaging decision, not an upstream removal: util-linux
  v2.41 still builds `login-utils/last.c` and its release notes contain no
  removal/deprecation. Debian `util-linux` packaging sets
  `--disable-last --disable-utmpdump` (comment `# => wtmpdb`); `debian/NEWS`
  (2.40.1-2): "last(1) has been split off to the wtmpdb package… lastb(1) is
  removed. Please see syslog/journal for failed login attempts."
  <https://raw.githubusercontent.com/util-linux/util-linux/v2.41/login-utils/meson.build>,
  <https://deb.debian.org/debian/pool/main/u/util-linux/>
- Trixie versions: util-linux 2.41, OpenSSH 10.0p1, systemd 257
  (release notes §2.2.10).

## wtmpdb

Upstream: <https://github.com/thkukuk/wtmpdb>. Y2038-safe wtmp replacement;
SQLite3 backend; data collected via PAM module `pam_wtmpdb.so` or `libwtmpdb`.

- Commands (Debian man page, wtmpdb 0.73.0): `last`, `boot`, `shutdown`,
  `rotate`, `import`. `wtmpdb last` supports `-n/--limit`, `-s/--since TIME`,
  `-i/--ip`, `-w/--fullnames`, `--time-format`, and username/tty positional
  filters. TIME formats: `YYYYMMDDHHMMSS`, `YYYY-MM-DD HH:MM:SS`,
  `YYYY-MM-DD HH:MM`, `YYYY-MM-DD`, `HH:MM:SS`, `HH:MM`, `now`, `today`,
  `yesterday`, `tomorrow`. **No relative `-Nm` format** (util-linux `last`
  has it; wtmpdb does not).
  <https://manpages.debian.org/trixie/wtmpdb/wtmpdb.8.en.html>
- Path: upstream default is `/var/lib/wtmpdb/wtmp.db`; **Debian patches it to
  `/var/log/wtmp.db`** (`db-in-var-log.patch`), with `/var/lib/wtmpdb/wtmp.db`
  a symlink to it. The article's `/var/log/wtmp.db` is right for Debian only.
- Who writes on Debian 13: **sshd directly** (openssh-server depends on
  `libwtmpdb0`, built `--with-wtmpdb`; upstream OpenSSH ≥ 10.0 has native
  support in `loginrec.c`), and **pam_wtmpdb** for other interactive PAM
  sessions (`skip_if=sshd` avoids double entries). Boot/shutdown entries come
  from `wtmpdb-update-boot.service`.
  <https://raw.githubusercontent.com/openssh/openssh-portable/V_10_0_P1/loginrec.c>
- Rotation on Debian is via logrotate (`/etc/logrotate.d/wtmpdb`: yearly,
  rotate 4), not `wtmpdb rotate`.
- **Permissions are not guaranteed.** Nothing in tmpfiles.d/postinst sets a
  mode on `/var/log/wtmp.db`; it is created by the first writer's
  `sqlite3_open()` with that writer's umask (root daemons/sshd: 022 → normally
  0644 root:root, world-readable). The restrictive `umask(0077)` in
  `lib/sqlite.c` applies only when built with wtmpdbd, which Debian disables.
  An unprivileged `last` therefore normally works on trixie, but a first
  writer with umask 077 would make it 0600. Tools must degrade gracefully.
- `last` on trixie is a **compat symlink to wtmpdb** (`debian/wtmpdb.links`),
  reading the same DB.
- Old `/var/log/wtmp`: imported into the DB on first install. Debian openssh
  is built `--with-wtmpdb` without `--disable-wtmp`, and upstream
  `record_login()` writes utmp + wtmp + wtmpdb in parallel, so sshd still
  appends `/var/log/wtmp` on trixie — but no default tool reads it (see
  unverified list).

## util-linux last (Debian ≤ 12, Ubuntu ≤ 24.10, RHEL 9/10)

Man page (util-linux): `-n/--limit`, `-s/--since TIME`, `-t/--until`,
`-i/--ip`, `-w/--fullnames`, `-F/--fulltimes`, `--time-format`, username/tty
positional filters. TIME also accepts relative `-number[smhd]` (e.g. `-5m`,
`-6h`, `-2d`). Reads `/var/log/wtmp` (btmp for `lastb`).
<https://man7.org/linux/man-pages/man1/last.1@@util-linux.html>

## rsyslog, journald and the SSH unit name

- **rsyslog is not installed by default on trixie** (`Priority: optional`);
  journald is the default log store. Debian's systemd ships a
  `ForwardToSyslog=yes` drop-in, but with no syslog daemon installed nothing
  reads that socket. The claim "syslog logs are often not kept nowadays" is
  confirmed for default Debian 13.
  <https://deb.debian.org/debian/pool/main/r/rsyslog/>
- SSH auth messages reach journald via syslog(3) (openssh `log.c`).
- RHEL-family rsyslog writes authpriv to **`/var/log/secure`**, not
  `/var/log/auth.log`: CentOS Stream 10 `rsyslog.conf` ships
  `authpriv.* action(type="omfile" file="/var/log/secure")`, and the package
  `%post` creates `/var/log/{messages,secure,maillog,spooler}`.
  <https://gitlab.com/redhat/centos-stream/rpms/rsyslog/-/raw/c10s/rsyslog.conf>,
  <https://gitlab.com/redhat/centos-stream/rpms/rsyslog/-/raw/c10s/rsyslog.spec>
- Unit name: Debian `openssh-server` ships `ssh.service` **with
  `Alias=sshd.service`** (active when enabled), plus `ssh.socket` and
  `sshd@.service` for socket activation. RHEL-family uses `sshd.service`.
  Ubuntu 25.04 release notes note the new `sshd.service` alias.
  <https://gitlab.com/redhat/centos-stream/rpms/openssh/-/raw/c10s/sshd.service>

## sshd log strings (openssh-portable source)

From `auth.c` `auth_log()` (V_9_9_P2 and master identical):
`authmsg = authenticated ? "Accepted" : "Failed"` (also `Partial` for
multi-method partial success); format
`"%s %s%s%s for %s%.100s from %.200s port %d ssh2%s%s"` with `"invalid user "`
inserted for unknown accounts:

- `Accepted publickey for ivan from 10.0.0.1 port 54321 ssh2: RSA SHA256:…`
- `Failed password for invalid user admin from 10.0.0.1 port 54321 ssh2`
- `Invalid user admin from 10.0.0.1 port 54321` — logged once per connection
  by `getpwnamallow()` when the account does not exist, *in addition to*
  subsequent `Failed password for invalid user …` lines.
- `Accepted` is logged for **every** auth method (`userauth_finish()` calls
  `auth_log()` regardless of method).
- Early disconnects: `Connection closed by authenticating user root 10.0.0.1
  port 5555 [preauth]`, `Connection closed by invalid user admin … [preauth]`,
  `Disconnected from authenticating user …` (packet.c/monitor.c). These appear
  only on abnormal/early disconnects, not every logout.
  <https://raw.githubusercontent.com/openssh/openssh-portable/V_9_9_P2/auth.c>
- **OpenSSH ≥ 9.8 identifier split:** the per-connection binary is
  `sshd-session`; auth messages are logged with `SYSLOG_IDENTIFIER=sshd-session`
  while the listener logs as `sshd`. Match **both** identifiers
  (`journalctl -t sshd -t sshd-session`), not just `-t sshd`. On ≤ 9.7
  everything is `sshd`.
  <https://raw.githubusercontent.com/openssh/openssh-portable/V_9_9_P2/sshd-session.c>

## w, utmp and loginctl

- `w` (procps-ng): "Usually w will use either the systemd sessions table or
  the utmp file to locate users"; `/var/run/utmp` is used **only on
  non-systemd hosts**. `-i/--ip-addr` is **ignored on systemd platforms**
  (libsystemd only provides the remote host, which may be a hostname or IP).
  The FROM field's presence depends on the procps build (major distros show
  it by default; `-f` toggles it).
  <https://man7.org/linux/man-pages/man1/w.1.html>
- systemd-logind does **not** write utmp/wtmp session records; it only reads
  utmp (inotify) to learn TTYs. Session records are written by the login
  programs themselves (sshd `loginrec.c`, agetty/login).
  <https://raw.githubusercontent.com/systemd/systemd/main/src/login/logind-utmp.c>
- Distro build change, not upstream removal: Debian builds systemd
  `-Dutmp=false` (no `/run/utmp` created). Ubuntu 25.04 release notes: systemd
  no longer built with utmp support; known bug LP#2103489 breaks `who`.
  Upstream systemd 258 removed only *runlevel* recording in utmp/wtmp.
  <https://documentation.ubuntu.com/release-notes/25.04>
- `loginctl` session properties (logind-session-dbus.c vtable): Id, User,
  Name, Timestamp, VTNr, Seat, TTY, Display, Remote, RemoteHost, RemoteUser,
  Service, Desktop, Scope, Leader, Audit, Type, Class, Active, State,
  IdleHint, IdleSinceHint, CanIdle, CanLock, LockedHint.
  <https://raw.githubusercontent.com/systemd/systemd/main/src/login/logind-session-dbus.c>,
  <https://www.freedesktop.org/software/systemd/man/latest/loginctl.html>
- **Unprivileged access:** property reads and session listing have no polkit
  action → an unprivileged user can `session-status`/`show-session` **other
  users' sessions**. Only management is gated (Terminate/Kill →
  `org.freedesktop.login1.manage`, Lock/Unlock → `lock-sessions`,
  auth_admin_keep; the session owner may always manage it).
  <https://raw.githubusercontent.com/systemd/systemd/main/src/login/org.freedesktop.login1.policy>

## sudo logging

- sudo opens syslog with identifier **"sudo"** (`plugins/sudoers/logging.c`),
  so `journalctl -t sudo` is the right query. Facility auth/authpriv,
  goodpri=notice, badpri=alert (sudoers `Defaults syslog*`). sudo also logs to
  the audit subsystem where supported.
  <https://github.com/sudo-project/sudo/blob/main/plugins/sudoers/logging.c>,
  <https://www.sudo.ws/docs/man/sudoers.man/>
- Caveat: Ubuntu 25.10 makes **sudo-rs** the default sudo; its logging
  specifics are unverified here.

## auditd and ausearch

- `USER_LOGIN` = audit record 1112, "User has logged in" (official message
  dictionary). Emitted by login programs via libaudit; Debian sshd is built
  `--with-audit=linux` and emits it (`audit-linux.c`
  `audit_log_acct_message(AUDIT_USER_LOGIN, …)`). `ausearch -m USER_LOGIN -i`
  needs no privileges itself — only read access to the log.
  <https://raw.githubusercontent.com/linux-audit/audit-documentation/main/specs/messages/message-dictionary.csv>,
  <https://raw.githubusercontent.com/openssh/openssh-portable/V_10_0_P1/audit-linux.c>,
  <https://man7.org/linux/man-pages/man8/ausearch.8.html>
- Permissions (audit 4.0.2 = trixie): upstream default 0600 root:root;
  **Debian's auditd.conf sets `log_group = adm`** → 0640 root:adm, directory
  0750 root:adm. A user in only `systemd-journal`+`docker` groups cannot read
  it; `adm` members can. auditd is `Priority: optional` — **not installed by
  default on Debian 13**.
  <https://raw.githubusercontent.com/linux-audit/audit-userspace/v4.0.2/src/auditd-event.c>,
  <https://man7.org/linux/man-pages/man5/auditd.conf.5.html>

## Tampering indicators: two are weak signals by themselves

- `Storage=auto` (journald default): behaves like `persistent` **iff
  `/var/log/journal` exists**, otherwise volatile; in auto mode journald does
  not create the directory. **On a default Debian 13 `/var/log/journal`
  legitimately does not exist** (journal in `/run/log/journal`, lost at
  reboot). Its absence is not evidence of tampering unless persistence was
  previously configured. When present: dir 2755 root:systemd-journal,
  `system.journal` 0640 root:systemd-journal (+ACL group:adm:r--).
  <https://www.freedesktop.org/software/systemd/man/latest/journald.conf.html>
- `/var/log/wtmp.db` absence: no tmpfiles entry creates it; it appears only
  when wtmpdb is installed and a first write happens. Absence primarily means
  "not installed / no recorded logins yet".
- The executable-unit warning is real: systemd `src/basic/fs-util.c`
  `log_warning("Configuration file %s is marked executable. Please remove
  executable permission bits. Proceeding anyway.", path)`. A unit file
  suddenly gaining +x is a legitimate oddity flag.
  <https://github.com/systemd/systemd/blob/v257/src/basic/fs-util.c>
- Shell-history clearing is a generic heuristic, not a distro fact.

## Compatibility matrix

Verified via Launchpad publishing history, Ubuntu official release notes and
CentOS Stream specs (proxy for RHEL).

| Distro | Active sessions | Login history | SSH journal unit / identifier | Unprivileged readability |
|---|---|---|---|---|
| Debian 13 trixie | logind; `w` uses logind, no utmp (`-Dutmp=false`) | SQLite `/var/log/wtmp.db` → `last` (symlink→wtmpdb) / `wtmpdb last`; old wtmp imported, still written by sshd but unread by default | `ssh.service` (Alias `sshd.service`), `ssh.socket`+`sshd@`; identifiers **sshd + sshd-session** | wtmp.db normally 0644 → `last` OK; journal via systemd-journal/adm; audit.log 0640 root:adm → NO |
| Debian 12 bookworm | logind + utmp (systemd 252) | `/var/log/wtmp` → util-linux `last`/`lastb`; `lastlog` | `ssh.service` | wtmp 0644 root:utmp → OK; journal volatile by default |
| Ubuntu 24.04 LTS | logind + utmp | util-linux 2.39.3 `last`/`lastb`/`lastlog`, `/var/log/wtmp` | `ssh.service` | OK |
| Ubuntu 24.10 | logind + utmp | util-linux 2.40.2, `last` still shipped | `ssh.service` | OK |
| Ubuntu 25.04 | logind; systemd utmp=false, `/run/utmp` absent (`who` broken, LP#2103489) | `last` **removed**, **no wtmpdb in archive**; journal only | `ssh.service` + `sshd.service` alias; OpenSSH 9.9 → sshd-session | journal via groups; no last at all |
| Ubuntu 25.10 | same as 25.04 | util-linux 2.41, no last, no wtmpdb; **sudo-rs default** | same; OpenSSH 10.0 | same |
| Ubuntu 26.04 LTS | logind | util-linux 2.41.3 (no last); wtmpdb 0.75.0 in universe (default seeding unverified); lastlog2 shipped | ssh/sshd alias | wtmpdb if installed |
| RHEL 9 (c9s) | logind + utmp | util-linux 2.37.4 `last`/`lastb`/`lastlog`, `/var/log/wtmp` | `sshd.service` | wtmp 0644 → OK |
| RHEL 10 (c10s) | logind + utmp | util-linux 2.40.2 `last`/`lastb`/`utmpdump`/`lastlog` | `sshd.service` | wtmp 0644 → OK |

Sources: <https://launchpad.net/ubuntu/+source/util-linux/+publishinghistory>,
<https://launchpad.net/ubuntu/+source/wtmpdb>,
<https://documentation.ubuntu.com/release-notes/25.04>,
<https://documentation.ubuntu.com/release-notes/25.10>,
<https://gitlab.com/redhat/centos-stream/rpms/util-linux/-/raw/c9s/util-linux.spec>,
<https://gitlab.com/redhat/centos-stream/rpms/util-linux/-/raw/c10s/util-linux.spec>

## Design consequences for MiniSRE

- Prefer `loginctl` for session facts; `w` adds idle/current-command context.
  Do not parse utmp directly.
- Detect `wtmpdb` first, fall back to `last`, fall back to journal-only
  (Ubuntu 25.04/25.10 have neither).
- Compute the `last -s` timestamp **on the host** (GNU `date`): wtmpdb accepts
  only absolute times, and the host clock/timezone is authoritative.
- Query the journal with `-t sshd -t sshd-session` (covers the ≥ 9.8 split
  and socket activation) and grep `Accepted|Failed|Invalid user`.
- Report storage facts (presence, mode, owner) neutrally; absent
  `/var/log/journal` or `wtmp.db` is not tamper evidence without a baseline.
- Inspect both wtmpdb locations (`/var/log/wtmp.db` on Debian,
  `/var/lib/wtmpdb/wtmp.db` upstream) and both rsyslog auth logs
  (`auth.log` on Debian-family, `secure` on RHEL-family); do not assert a
  distro-specific path in labels.
- Distinguish ENOENT from EACCES when stat'ing storage: Debian's
  `/var/log/audit` is 0750 root:adm, so an unprivileged stat fails with
  Permission denied — that is unknown status, not absence.
- audit.log is normally unreadable by the mini-sre user: report a limitation,
  never attempt privilege escalation.

## Still unverified

1. Whether a Debian openssh patch (beyond build flags) suppresses the parallel
   `/var/log/wtmp` write on trixie.
2. Whether shadow `login`/agetty still write wtmp/utmp for console logins on
   trixie.
3. Actual on-disk mode of `wtmp.db` on a given host (first writer's umask).
4. Whether Ubuntu 25.04/25.10 sshd still writes `/var/log/wtmp` (build flags
   not fetched).
5. sudo-rs syslog identifier/defaults (Ubuntu 25.10+ default sudo).
6. Whether wtmpdb is seeded by default in Ubuntu 26.04.
7. RHEL-proper (vs CentOS Stream) package parity; RHEL 9/10 auditd
   default-install status.
8. Exact Debian 12 util-linux version (classic wtmp stack asserted).
9. USER_LOGIN field-level schema (`op= id= exe= hostname= addr= terminal= res=`).
10. Runtime polkit behavior (conclusions drawn from policy file + vtable
    source only; no live-host checks were performed).

Method note: packages.debian.org / sources.debian.org / salsa.debian.org
served bot-challenges, so Debian packaging facts were taken from the official
package files on deb.debian.org (control, postinst, rules, patches, tmpfiles,
units) and manpages.debian.org; both are primary sources.
