// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import "./testkit";
import { PhoneAccess } from "./PhoneAccess";
import { PhonePop } from "./PhonePop";
import type { HubPort } from "../port/hub";
import type { ShareStatus } from "../port/share";

afterEach(cleanup);

// The regression the closed-share black screen came from: a closed share's
// door set arrived as null, and drawing the card read it as a list. The
// fixture keeps the wire's exact shape — doors absent-marshal, i.e. null.
const closedFromKernel = {
  open: false,
  doors: null,
  addresses: [{ ip: "10.0.0.2", interface: "en0", kind: "lan" }],
  devices: [],
  cloudDevices: [],
} as unknown as ShareStatus;

it("opens the card on a closed share whose doors arrived as null", async () => {
  const hub = {
    shareStatus: vi.fn(async () => closedFromKernel),
    offerShare: vi.fn(),
    shareRemember: vi.fn(async () => ({ remember: false })),
  } as unknown as HubPort;
  render(<PhonePop hub={hub} />);
  await userEvent.click(await screen.findByRole("button", { name: "设备访问" }));
  const dialog = await screen.findByRole("dialog");
  // The card draws the shut door it knows: master switch off, mint waiting
  // for the share to be opened, and no trace of a door list.
  expect(dialog.querySelector('[data-action="share.toggle"]')).not.toBeNull();
  expect(dialog.querySelector('[data-action="share.offer"]')).toBeNull();
  await waitFor(() => expect(hub.shareStatus).toHaveBeenCalled());
});

it("the settings block survives the same null door set", async () => {
  const hub = {
    shareStatus: vi.fn(async () => closedFromKernel),
    offerShare: vi.fn(),
    shareRemember: vi.fn(async () => ({ remember: false })),
  } as unknown as HubPort;
  render(<PhoneAccess hub={hub} onError={() => {}} />);
  // The master switch row draws; nothing throws reading the null doors.
  await waitFor(() => expect(document.querySelector('[data-action="share.toggle"]')).not.toBeNull());
});
