// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { cleanup, render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import "./testkit";
import { keepLiveOffers, PhoneAccess } from "./PhoneAccess";
import type { ShareOffer, ShareStatus } from "../port/share";

afterEach(cleanup);

const base: ShareStatus = {
  open: false, doors: [], addresses: [{ ip: "10.0.0.2", interface: "en0", kind: "lan" }], devices: [], cloudDevices: [],
};

function rig(initial: ShareStatus) {
  let st = initial;
  const hub = {
    shareStatus: vi.fn(async () => st),
    setSharePort: vi.fn(async (port: number) => (st = { ...st, port: port || undefined })),
    openShare: vi.fn(async (ip: string) => {
      const door = { address: ip, interface: "en0", kind: "lan" as const, origin: `http://${ip}:${st.port ?? 50000}` };
      st = { ...st, open: true, doors: st.doors.some((d) => d.address === ip) ? st.doors : [...st.doors, door] };
      return st;
    }),
    offerShare: vi.fn(async (ip: string) => ({ url: `http://${ip}/#pair=x`, qr: "<svg/>", expires: new Date(Date.now() + 60_000).toISOString() })),
    shareRemember: vi.fn(async () => ({ remember: false })),
  };
  return { hub, ui: <PhoneAccess hub={hub as unknown as HubPort} onError={() => {}} /> };
}

const portField = () =>
  waitFor(() => {
    const el = document.querySelector<HTMLInputElement>('[data-action="share.port"]');
    expect(el).not.toBeNull();
    return el!;
  });

it("saves a typed port when the field loses focus and leaves a shut door shut", async () => {
  const { hub, ui } = rig(base);
  render(ui);
  const field = await portField();
  await userEvent.type(field, "41234");
  await userEvent.tab();
  await waitFor(() => expect(hub.setSharePort).toHaveBeenCalledWith(41234));
  expect(hub.openShare).not.toHaveBeenCalled();
});

it("clearing the field sends zero, and an unchanged field sends nothing", async () => {
  const { hub, ui } = rig({ ...base, port: 41234 });
  render(ui);
  const field = await portField();
  expect(field.value).toBe("41234");
  await userEvent.click(field);
  await userEvent.tab();
  expect(hub.setSharePort).not.toHaveBeenCalled();
  await userEvent.clear(field);
  await userEvent.tab();
  await waitFor(() => expect(hub.setSharePort).toHaveBeenCalledWith(0));
});

// A standing door is not moved onto the new port: the phone that paired on
// the old origin keeps its cookie, and the port waits for the next open.
it("saves the port without moving a standing door", async () => {
  const { hub, ui } = rig({
    ...base,
    open: true,
    doors: [{ address: "10.0.0.2", interface: "en0", kind: "lan", origin: "http://10.0.0.2:50000" }],
  });
  render(ui);
  const field = await portField();
  await userEvent.type(field, "41234");
  await userEvent.tab();
  await waitFor(() => expect(hub.setSharePort).toHaveBeenCalledWith(41234));
  expect(hub.openShare).not.toHaveBeenCalled();
});

const offer = (ip: string): ShareOffer => ({ url: `http://${ip}/#pair=x`, qr: "<svg/>", expires: new Date().toISOString() });

const door = (ip: string, offerExpires?: string) => ({ address: ip, interface: "en0", kind: "lan" as const, origin: `http://${ip}:50000`, offerExpires });

// A spent code's picture goes, and the door beside it keeps its own.
it("keeps each door's live picture and drops the spent one", () => {
  const st: ShareStatus = {
    ...base,
    open: true,
    doors: [door("10.0.0.2", undefined), door("100.64.0.1", new Date().toISOString())],
  };
  const kept = keepLiveOffers({ "10.0.0.2": offer("10.0.0.2"), "100.64.0.1": offer("100.64.0.1") }, st);
  expect(Object.keys(kept)).toEqual(["100.64.0.1"]);
});

it("keeps a picture whose door still holds its clock", () => {
  const st: ShareStatus = { ...base, open: true, doors: [door("10.0.0.2", new Date().toISOString())] };
  const kept = keepLiveOffers({ "10.0.0.2": offer("10.0.0.2") }, st);
  expect(Object.keys(kept)).toEqual(["10.0.0.2"]);
});

it("keeps nothing when no door stands", () => {
  const kept = keepLiveOffers({ "10.0.0.2": offer("10.0.0.2") }, base);
  expect(kept).toEqual({});
});
