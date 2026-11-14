import { AndroidIcon, AppleIcon, IOSIcon, LinuxIcon, WindowsIcon } from "@/components/OsIcons";

/**
 * The platform cards of the device profile editor (ADR-031 D-5, D-10), in
 * the order of the "Approved Operating Systems" grid. `comingSoon`: no
 * client for that platform yet, so the card can't be turned on.
 */
export const PROFILE_PLATFORMS = [
  { id: "windows", label: "Windows", Icon: WindowsIcon, comingSoon: false },
  { id: "macos", label: "macOS", Icon: AppleIcon, comingSoon: true },
  { id: "linux", label: "Linux", Icon: LinuxIcon, comingSoon: false },
  { id: "ios", label: "iOS", Icon: IOSIcon, comingSoon: false },
  { id: "android", label: "Android", Icon: AndroidIcon, comingSoon: false },
] as const;

export type ProfilePlatformId = (typeof PROFILE_PLATFORMS)[number]["id"];

/** ADR-031 D-11: the Minimum OS Version checks take a minimum version. */
export function isMinOSCheck(checkId: string): boolean {
  return checkId.endsWith(".os.minimum");
}

/** Mirrors the controller's NormalizeMinVersion: 1–4 dot-separated numbers. */
export function validMinVersion(version: string): boolean {
  return /^\d{1,9}(\.\d{1,9}){0,3}$/.test(version.trim());
}
