import { Laptop, Monitor, Smartphone } from "lucide-react";

/** The platforms the device profile editor offers cards for (ADR-031 D-5). */
export const PROFILE_PLATFORMS = [
  { id: "linux", label: "Linux", Icon: Laptop },
  { id: "android", label: "Android", Icon: Smartphone },
  { id: "windows", label: "Windows", Icon: Monitor },
] as const;

export type ProfilePlatformId = (typeof PROFILE_PLATFORMS)[number]["id"];
