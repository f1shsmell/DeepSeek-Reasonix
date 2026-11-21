import { useCallback, useEffect, useState } from "react";
import { t } from "../i18n";
import type { HubPort } from "../port/hub";
import type { CloudShareOffer, ShareAddress, ShareOffer, ShareStatus } from "../port/share";
import { copyText } from "./CopyButton";
import { Switch } from "./Switch";

interface Props {
  hub: HubPort;
  onError: (e: unknown) => void;
}

// While a door is open the list is what changes under the reader: a phone
// pairs, the code on screen is spent. Nothing pushes that, so it is read.
const POLL_MS = 3000;

export function clock(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? "" : d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

// A device is named by when it paired. Its user agent is kept, as the title,
// but read as a name it is a hundred characters and every one starts the same.
export const deviceLabel = (i: number) => t("remote.device_n", { n: i + 1 });

// One line a reader tells addresses apart by: the adapter a phone would dial.
export const addressLabel = (a: ShareAddress) =>
  a.kind === "tailnet"
    ? `${a.ip} · Tailscale`
    : a.kind === "virtual"
      ? `${a.ip} · ${a.interface} · ${t("remote.virtual_adapter")}`
      : `${a.ip} · ${a.interface}`;

export type Share = ReturnType<typeof useShare>;

// Prunes the pictures of codes the kernel no longer holds: each door keeps
// its own, so one door's spend does not drop another's. Pure, so the pruning
// is tested without a render.
export function keepLiveOffers(prev: Record<string, ShareOffer>, st: ShareStatus): Record<string, ShareOffer> {
  const kept: Record<string, ShareOffer> = {};
  for (const [ip, offer] of Object.entries(prev)) {
    if (st.doors.some((d) => d.address === ip && d.offerExpires)) kept[ip] = offer;
  }
  return kept;
}

/** The window's door for phones, as one state both the settings block and the
 *  chrome's card draw from. `watch` is whether this surface is on screen: only
 *  then is the device list worth reading on a timer. */
export function useShare(hub: HubPort, onError: (e: unknown) => void, watch = true) {
  const [st, setSt] = useState<ShareStatus | null>(null);
  const [offers, setOffers] = useState<Record<string, ShareOffer>>({});
  const [cloudOffer, setCloudOffer] = useState<CloudShareOffer | null>(null);
  const [busy, setBusy] = useState(false);
  const [confirm, setConfirm] = useState("");
  const [remember, setRemember] = useState<boolean | null>(null);

  const read = useCallback(
    () =>
      hub.shareStatus().then((now) => {
        setSt(now);
        return now;
      }),
    [hub],
  );

  useEffect(() => {
    read().catch(() => setSt(null));
  }, [read]);

  useEffect(() => {
    if (!watch) return;
    void read().catch(() => {});
    const timer = setInterval(() => read().catch(() => {}), POLL_MS);
    return () => clearInterval(timer);
  }, [watch, read]);

  // The kernel withdraws a code once it pairs or lapses; the picture of it
  // has to go too, or the next phone scans a code that can no longer work.
  // Every door keeps its own, so one door's spend does not drop another's.
  useEffect(() => {
    if (!st) return;
    setOffers((prev) => keepLiveOffers(prev, st));
  }, [st]);

  useEffect(() => {
    if (cloudOffer && st && !st.cloudRemote?.online) setCloudOffer(null);
  }, [st, cloudOffer]);

  const run = useCallback(
    async (step: () => Promise<void>) => {
      setBusy(true);
      try {
        await step();
      } catch (e) {
        onError(e);
      } finally {
        setBusy(false);
      }
    },
    [onError],
  );

  useEffect(() => {
    hub.shareRemember().then((r) => setRemember(r?.remember ?? false)).catch(() => setRemember(null));
  }, [hub]);

  const saveRemember = useCallback(
    (on: boolean) => run(async () => setRemember((await hub.setShareRemember(on)).remember)),
    [run, hub],
  );

  // The code and the status that knows it is on offer land together; set one
  // at a time, the effect above reads a fresh code against a status that has
  // not heard of it yet and drops it.
  const mint = useCallback(
    async (ip: string) => {
      const next = await hub.offerShare(ip);
      const now = await hub.shareStatus();
      setSt(now);
      setOffers((prev) => ({ ...prev, [ip]: next }));
    },
    [hub],
  );

  // Opening a door asks for its code at once: the row is what the reader
  // wanted to see. Closing one drops that door's picture with the door; the
  // other doors and the paired phones are not touched.
  const toggleAddress = useCallback(
    (ip: string, on: boolean) =>
      run(async () => {
        if (on) {
          await hub.openShare(ip);
          await mint(ip);
          return;
        }
        setOffers((prev) => {
          const { [ip]: _gone, ...rest } = prev;
          return rest;
        });
        setSt(await hub.closeShare(ip));
      }),
    [hub, mint, run],
  );

  // The master switch: on opens the first address this machine offers; off
  // closes every door and unpairs every phone.
  const toggle = useCallback(
    () =>
      run(async () => {
        if (st?.open) {
          setOffers({});
          setSt(await hub.closeShare());
          return;
        }
        const at = st?.addresses[0]?.ip ?? "";
        if (!at) return;
        await hub.openShare(at);
        await mint(at);
      }),
    [hub, mint, run, st],
  );

  // A fixed port is read by the doors opened next; a standing door is not
  // moved, so a phone that paired on it keeps its cookie.
  const savePort = useCallback(
    (port: number) => run(async () => setSt(await hub.setSharePort(port))),
    [hub, run],
  );

  const newCode = useCallback((ip: string) => run(() => mint(ip)), [run, mint]);
  const newCloudCode = useCallback(
    () => run(async () => setCloudOffer(await hub.offerCloudShare())),
    [hub, run],
  );

  const revoke = (id: string) => run(async () => setSt(await hub.revokeDevice(id)));

  return { st, offers, savePort, cloudOffer, busy, confirm, setConfirm, newCode, newCloudCode, toggle, toggleAddress, revoke, remember, saveRemember, refresh: read };
}

/** The switch, the doors, each door's code, and the paired phones. */
export function ShareBody({ share }: { share: Share }) {
  const { st, offers, savePort, busy, confirm, setConfirm, newCode, toggle, toggleAddress, revoke, remember, saveRemember } = share;
  const [copied, setCopied] = useState<"" | "done" | "failed">("");
  const [portDraft, setPortDraft] = useState<string | null>(null);
  if (!st) return null;
  // The link the code draws, for a device that cannot scan: the same
  // one-time credential, so it is copied on request and never shown as text.
  const copyLink = (url: string) =>
    copyText(url)
      .then(() => setCopied("done"))
      .catch(() => setCopied("failed"))
      .finally(() => window.setTimeout(() => setCopied(""), 1600));
  const noNetwork = st.addresses.length === 0;
  const commitPort = () => {
    if (portDraft === null) return;
    const next = portDraft === "" ? 0 : Number(portDraft);
    setPortDraft(null);
    if (next !== (st.port ?? 0)) void savePort(next);
  };

  return (
    <>
      <div className="lrow">
        <span className="tx">
          <span className="lb">{t("remote.allow_phones_to_connect")}</span>
          <span className="ds">
            {noNetwork
              ? t("remote.this_computer_has_no_private")
              : t("remote.a_phone_on_the_same")}
          </span>
        </span>
        <Switch data-action="share.toggle" on={st.open} busy={busy || noNetwork} label={t("remote.allow_phones_to_connect")} onClick={() => void toggle()} />
      </div>

      <div className="lrow">
        <span className="tx">
          <span className="lb">{t("remote.remember_paired_phones")}</span>
          <span className="ds">
            {t("remote.when_on_a_restart_reopens")}
          </span>
        </span>
        <Switch data-action="share.remember" on={remember ?? false} busy={busy || remember === null} label={t("remote.remember_paired_phones")} onClick={() => void saveRemember(!(remember ?? false))} />
      </div>

      {!noNetwork && (
        <div className="sharedoors">
          <p className="rmthint">{t("remote.every_network_has_its_own")}</p>
          {st.addresses.map((a) => {
            const door = st.doors.find((d) => d.address === a.ip);
            const offer = door ? offers[a.ip] : undefined;
            return (
              <div className="sharedoor" key={a.ip} data-on={door ? "" : undefined}>
                <div className="sharedoorhd">
                  <span className="lb" title={a.ip}>{addressLabel(a)}</span>
                  <Switch
                    data-action="share.door"
                    data-target={a.ip}
                    on={!!door}
                    busy={busy}
                    label={t("remote.allow_this_address")}
                    onClick={() => toggleAddress(a.ip, !door)}
                  />
                </div>
                {door &&
                  (offer ? (
                    <div className="shareqr">
                      <img src={`data:image/svg+xml;charset=utf-8,${encodeURIComponent(offer.qr)}`} alt={t("remote.pairing_qr_code")} width={184} height={184} />
                      <div className="sharehow">
                        <span className="lb">{t("remote.scan_with_your_phones_camera")}</span>
                        <span className="ds">{t("remote.the_code_pairs_one_phone", { time: clock(offer.expires) })}</span>
                        <code dir="ltr">{door.origin}</code>
                        <button className="rmtlnk sharecopy" data-action="share.copy" data-target={a.ip} onClick={() => void copyLink(offer.url)}>
                          {copied === "done" ? t("common.copied") : copied === "failed" ? t("remote.copy_blocked") : t("remote.copy_pairing_link")}
                        </button>
                      </div>
                    </div>
                  ) : (
                    <button className="rmtadd" data-action="share.offer" data-target={a.ip} disabled={busy} onClick={() => void newCode(a.ip)}>
                      {t("remote.show_a_pairing_code")}
                    </button>
                  ))}
              </div>
            );
          })}
          {st.open && <p className="rmthint">{t("remote.turning_a_network_off_stops_only")}</p>}
        </div>
      )}

      <label className="rmtf">
        <span>{t("common.port")}</span>
        <input
          data-action="share.port"
          inputMode="numeric"
          maxLength={5}
          value={portDraft ?? (st.port ? String(st.port) : "")}
          placeholder={t("common.auto")}
          disabled={busy}
          onChange={(e) => setPortDraft(e.target.value.replace(/\D/g, ""))}
          onBlur={commitPort}
        />
      </label>
      <p className="rmthint">
        {t("remote.leave_empty_to_let_the")}
      </p>

      {st.open && st.devices.length > 0 && (
        <div className="sharedevs">
          {st.devices.map((d, i) => (
            <div className="sharedev" key={d.id}>
              <span className="nm" title={d.name}>{deviceLabel(i)}</span>
              <span className="st">{d.online ? t("remote.paired_time_online", { time: clock(d.pairedAt) }) : t("remote.paired_time_last_seen_seen", { time: clock(d.pairedAt), seen: clock(d.lastSeen) })}</span>
              {confirm === d.id ? (
                <span className="rmtconfirm" role="alertdialog">
                  <span>{t("remote.disconnect_this_phone_it_will")}</span>
                  <button data-action="share.keep" data-target={d.id} onClick={() => setConfirm("")}>{t("common.cancel")}</button>
                  <button
                    data-danger=""
                    data-action="share.revoke"
                    data-target={d.id}
                    autoFocus
                    onClick={() => {
                      setConfirm("");
                      void revoke(d.id);
                    }}
                  >
                    {t("remote.disconnect")}
                  </button>
                </span>
              ) : (
                <button className="rmtlnk" data-danger="" data-action="share.ask-revoke" data-target={d.id} onClick={() => setConfirm(d.id)}>
                  {t("remote.disconnect")}
                </button>
              )}
            </div>
          ))}
        </div>
      )}
      {st.open && st.devices.length === 0 && <p className="rmtempty">{t("remote.no_phone_is_connected_yet")}</p>}

      <p className="sharewarn">
        {t("remote.the_connection_is_plain_http")}
      </p>
    </>
  );
}

/** A phone on the same network drives this window's panes. Nothing here is
 *  kept: the door shuts with the window, and shutting it unpairs every phone. */
export function PhoneAccess({ hub, onError }: Props) {
  const share = useShare(hub, onError);
  // A settings page must not grow a heading for a door this kernel cannot open.
  if (!share.st) return null;
  return (
    <div className="phone-door">
      <div className="grp-hd">
        <h3>{t("remote.phone_access")}</h3>
      </div>
      <div className="grp-items share">
        <ShareBody share={share} />
      </div>
    </div>
  );
}
