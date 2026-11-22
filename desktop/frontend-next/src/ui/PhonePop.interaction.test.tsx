// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import "./testkit";
import { PhonePop } from "./PhonePop";
import { HttpError } from "../port/http_error";
import type { HubPort } from "../port/hub";
import type { ShareStatus } from "../port/share";

afterEach(cleanup);

const status = (open: boolean): ShareStatus => ({
  open,
  doors: open
    ? [{ address: "10.0.0.2", interface: "en0", kind: "lan", origin: "http://10.0.0.2:50000" }]
    : [],
  addresses: [{ ip: "10.0.0.2", interface: "en0", kind: "lan" }],
  devices: [],
  cloudDevices: [],
});

function hubWith(reads: ShareStatus[], offerShare: () => Promise<unknown>) {
  let at = 0;
  const shareStatus = vi.fn(async () => reads[Math.min(at++, reads.length - 1)]);
  return {
    hub: {
      shareStatus,
      offerShare: vi.fn(offerShare),
      shareRemember: vi.fn(async () => ({ remember: false })),
      setShareRemember: vi.fn(async (on: boolean) => ({ remember: on })),
    } as unknown as HubPort,
    shareStatus,
  };
}

const closed = new HttpError(409, "share closed", { code: "share.closed" });

// The card asks for no code of its own on opening: a LAN code is one
// person's to spend, and its mint waits for the press on its own row.
it("does not ask for a code on opening, whatever the read finds", async () => {
  const { hub, shareStatus } = hubWith([status(true), status(true), status(true)], () => Promise.resolve({ url: "", qr: "", expires: "" }));
  render(<PhonePop hub={hub} />);
  await waitFor(() => expect(shareStatus).toHaveBeenCalledTimes(2));
  await userEvent.click(screen.getByRole("button", { name: "设备访问" }));
  await waitFor(() => expect(shareStatus).toHaveBeenCalledTimes(3));
  expect(hub.offerShare).not.toHaveBeenCalled();
  expect(screen.queryByRole("alert")).toBeNull();
});

// A failure in the card is said in the card, not somewhere else on screen.
it("says a failed code request inside the card", async () => {
  const { hub } = hubWith([status(true)], () => Promise.reject(closed));
  render(<PhonePop hub={hub} />);
  await userEvent.click(await screen.findByRole("button", { name: "设备访问" }));
  await userEvent.click(await screen.findByRole("button", { name: "显示配对二维码" }));
  const dialog = await screen.findByRole("dialog");
  await waitFor(() => expect(dialog.querySelector('[role="alert"]')?.textContent).toBe("手机访问已关闭，请先开启。"));
});

it("mints the code for the door its button names", async () => {
  const two: ShareStatus = {
    ...status(true),
    doors: [
      { address: "10.0.0.2", interface: "en0", kind: "lan", origin: "http://10.0.0.2:50000" },
      { address: "100.101.7.3", interface: "Tailscale", kind: "tailnet", origin: "http://100.101.7.3:50000" },
    ],
    addresses: [
      { ip: "10.0.0.2", interface: "en0", kind: "lan" },
      { ip: "100.101.7.3", interface: "Tailscale", kind: "tailnet" },
    ],
  };
  const { hub } = hubWith([two, two], () => Promise.resolve({ url: "", qr: "", expires: "" }));
  render(<PhonePop hub={hub} />);
  await userEvent.click(await screen.findByRole("button", { name: "设备访问" }));
  const buttons = await screen.findAllByRole("button", { name: "显示配对二维码" });
  expect(buttons).toHaveLength(2);
  await userEvent.click(buttons[1]);
  await waitFor(() => expect(hub.offerShare).toHaveBeenCalledWith("100.101.7.3"));
});

it("disconnects an Internet controller after the same two-step confirmation", async () => {
  const remote = {
    ...status(false),
    cloudDevices: [{ id: "cloud-a", name: "Web Studio", connectedAt: new Date().toISOString(), lastSeen: new Date().toISOString(), ordinal: 1 }],
  };
  const revokeDevice = vi.fn(async () => ({ ...remote, cloudDevices: [] }));
  const hub = {
    shareStatus: vi.fn(async () => remote),
    offerShare: vi.fn(),
    revokeDevice,
    shareRemember: vi.fn(async () => ({ remember: false })),
  } as unknown as HubPort;
  render(<PhonePop hub={hub} />);
  await userEvent.click(await screen.findByRole("button", { name: "设备访问" }));
  const disconnect = await screen.findByRole("button", { name: "断开" });
  await userEvent.click(disconnect);
  expect(screen.getByRole("button", { name: "确认断开" })).toBeTruthy();
  await userEvent.click(screen.getByRole("button", { name: "确认断开" }));
  await waitFor(() => expect(revokeDevice).toHaveBeenCalledWith("cloud-a"));
});

it("shows the account-gated Internet QR, and asks no LAN code for it", async () => {
  const remote = {
    ...status(false),
    cloudRemote: { deviceId: "device-a", name: "Home Mac", online: true },
  };
  const offerCloudShare = vi.fn(async () => ({
    url: "https://reasonix.io/remote/?device=device-a",
    qr: '<svg xmlns="http://www.w3.org/2000/svg"/>',
  }));
  const offerShare = vi.fn();
  const hub = {
    shareStatus: vi.fn(async () => remote),
    offerCloudShare,
    offerShare,
    shareRemember: vi.fn(async () => ({ remember: false })),
  } as unknown as HubPort;
  render(<PhonePop hub={hub} />);
  await userEvent.click(await screen.findByRole("button", { name: "设备访问" }));
  expect(await screen.findByRole("img", { name: "互联网连接二维码" })).toBeTruthy();
  expect(screen.getByText("手机扫码 · 不在同一网络也能连接")).toBeTruthy();
  const code = document.querySelector(".pc-code")!;
  expect(code.querySelector(":scope > img")).not.toBeNull();
  expect(code.querySelector(":scope > .pc-side .pc-when")).not.toBeNull();
  expect(code.querySelector(":scope > .pc-side .pc-acts")).not.toBeNull();
  expect(offerCloudShare).toHaveBeenCalledTimes(1);
  expect(offerShare).not.toHaveBeenCalled();
});

it.each([
  [undefined, "登录 Reasonix 账号后，可生成在外网也能使用的连接二维码。"],
  ["signed_out", "登录 Reasonix 账号后，可生成在外网也能使用的连接二维码。"],
  // A machine with no relay configured must not be told to sign in: its
  // answer is an unset address, and the fix is the environment variable.
  ["relay_unset", "这台电脑还没有配置中转地址，请把 REASONIX_REMOTE_URL 指向你的自建服务后重启。"],
  ["relay_unreachable", "中转服务暂时无法连接，请检查网络或代理设置后重试。"],
  ["relay_refused", "中转服务拒绝了这台设备的连接，请稍后重试。"],
] as const)("explains why Internet access is unavailable: %s", async (reason, note) => {
  const remote = { ...status(false), cloudRemote: { online: false, reason, error: "account: not signed in" } };
  const hub = { shareStatus: vi.fn(async () => remote), offerShare: vi.fn(), shareRemember: vi.fn(async () => ({ remember: false })) } as unknown as HubPort;
  render(<PhonePop hub={hub} />);
  await userEvent.click(await screen.findByRole("button", { name: "设备访问" }));
  expect(await screen.findByText(note)).toBeTruthy();
  if (reason === "relay_unset" || reason === "relay_unreachable" || reason === "relay_refused") {
    expect(screen.queryByText("登录 Reasonix 账号后，可生成在外网也能使用的连接二维码。")).toBeNull();
  }
});

it("labels Internet controllers by their ordinal, not their list position", async () => {
  const now = new Date().toISOString();
  const cloud = (id: string, ordinal: number) => ({ id, name: "Web Studio", connectedAt: now, lastSeen: now, ordinal });
  const remote = { ...status(false), cloudDevices: [cloud("b", 2), cloud("c", 3)] };
  const hub = { shareStatus: vi.fn(async () => remote), offerShare: vi.fn(), revokeDevice: vi.fn(), shareRemember: vi.fn(async () => ({ remember: false })) } as unknown as HubPort;
  render(<PhonePop hub={hub} />);
  await userEvent.click(await screen.findByRole("button", { name: "设备访问" }));
  await screen.findByText("设备 2");
  expect(screen.getByText("设备 3")).toBeTruthy();
  expect(screen.queryByText("设备 1")).toBeNull();
});
