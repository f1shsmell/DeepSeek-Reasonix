// The window's door for phones: a second listener on a private address that a
// paired device reaches the same panes through. Mirrors serve.ShareStatus,
// serve.ShareOffer and serve.DeviceView.

// lan is Wi-Fi or Ethernet; virtual is a VPN or proxy tunnel a phone usually
// cannot reach. The kernel lists them in that order, so the first is the default.
export type AddressKind = "lan" | "tailnet" | "virtual";

export interface ShareAddress {
  interface: string;
  ip: string;
  kind: AddressKind;
}

export interface PairedDevice {
  id: string;
  name: string;
  pairedAt: string;
  lastSeen: string;
  // Holding an event stream open now: a page on screen.
  online: boolean;
}

export interface CloudDevice {
  id: string;
  name: string;
  connectedAt: string;
  lastSeen: string;
  ordinal: number;
}

export interface CloudRemoteStatus {
  deviceId?: string;
  name?: string;
  online: boolean;
  error?: string;
  reason?: "signed_out" | "relay_unset" | "relay_unreachable" | "relay_refused";
}

// A paired device's answer about itself. Mirrors serve.DeviceSelf.
export interface DeviceSelf {
  id: string;
  // The number the window lists it under, so both say 设备 N.
  ordinal: number;
  machine: string;
}

// One address the share answers on: the adapter a phone dials, the origin it
// is addressed by, and that door's own pairing code's clock.
export interface ShareDoor {
  address: string;
  interface?: string;
  kind: AddressKind;
  origin: string;
  offerExpires?: string;
}

export interface ShareStatus {
  open: boolean;
  // The fixed port the user chose; absent means the system picks one each time.
  port?: number;
  // The addresses currently answering, in the order they opened. A share may
  // answer on more than one; every door holds its own pairing code.
  doors: ShareDoor[];
  addresses: ShareAddress[];
  devices: PairedDevice[];
  cloudDevices: CloudDevice[];
  cloudRemote?: CloudRemoteStatus;
}

export interface ShareOffer {
  url: string;
  // An SVG document, drawn dark on light whatever the theme so it scans.
  qr: string;
  expires: string;
}

export interface CloudShareOffer {
  url: string;
  qr: string;
}

// One device (a phone's browser) enrolled against this Studio at the account
// service. Mirrors serve.PhoneView. pending is waiting for the person here.
export interface PhoneView {
  id: string;
  ordinal?: number;
  name: string;
  state: "pending" | "active" | "revoked";
  uaClass: string;
  createdAt: string;
  lastSeenAt: string;
  expiresAt?: string;
  // What to compare when two devices look alike.
  keyThumbprint: string;
  // The active device a pending one says it replaces.
  replaceCandidate?: string;
}

export interface PhoneList {
  phones: PhoneView[];
}

export type PhoneAction = "add" | "replace" | "reject";

export interface SharePort {
  // Null where this kernel has no window to share from: a browser tab, or a
  // phone that is itself paired.
  shareStatus(): Promise<ShareStatus | null>;
  openShare(ip: string): Promise<ShareStatus>;
  // An address closes that one door; none closes the whole share.
  closeShare(ip?: string): Promise<ShareStatus>;
  // Zero clears the choice. Used by the doors opened next.
  setSharePort(port: number): Promise<ShareStatus>;
  // Whether this install keeps the phones that paired across restarts. Null
  // where the reading failed: the share routes exist only on a window's kernel.
  shareRemember(): Promise<{ remember: boolean } | null>;
  setShareRemember(remember: boolean): Promise<{ remember: boolean }>;
  offerShare(ip: string): Promise<ShareOffer>;
  offerCloudShare(): Promise<CloudShareOffer>;
  revokeDevice(id: string): Promise<ShareStatus>;
  // The devices enrolled against this Studio at the account service. Null where
  // this kernel has no window to ask from; a refusal is a thrown coded error.
  sharePhones(): Promise<PhoneList | null>;
  // Answers a waiting device, and answers with the list as it is afterwards.
  resolvePhone(id: string, action: PhoneAction, replaceId?: string): Promise<PhoneList>;
  revokePhone(id: string): Promise<PhoneList>;
  // What this page is to the kernel: a paired device, or null for the window
  // and for a browser on a networked serve.
  device(): Promise<DeviceSelf | null>;
  // Unpairs this device from its own side.
  leaveDevice(): Promise<void>;
}
