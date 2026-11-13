//! Windows access control for the client: who may use the daemon's IPC pipe,
//! who may read the data directory (config, encrypted state and its key), and
//! how the CLI tells the real daemon's pipe from an impostor.
//!
//! The Linux client gets all of this from the OS for free: the daemon runs as
//! the enrolling user, its state lives in that user's 0700 home directory and
//! its socket in a 0700 RuntimeDirectory. On Windows the daemon runs as
//! LocalSystem, `C:\ProgramData` lets any user create folders, and pipe names
//! are first-come, so each guarantee is made explicit here:
//!
//! - the pipe's DACL admits SYSTEM, Administrators and the enrolling user;
//! - the service creates the pipe owned by SYSTEM, and the CLI refuses a pipe
//!   owned by anyone but SYSTEM or Administrators (no squatting while the
//!   service is stopped);
//! - the daemon refuses a data directory, or anything in it, that is a
//!   reparse point (junction, symlink) or is owned by anyone but SYSTEM or
//!   Administrators (no pre-created `C:\ProgramData\Zecurity`), and sets a
//!   protected DACL on each directory before reading anything;
//! - the enrolled user can read, and delete stale state files, but can never
//!   create, rename or replace anything there. A user who could would be able
//!   to swap `state` for a junction and have the SYSTEM daemon write, or set
//!   ACLs, wherever it points. Config changes (`setup`) need an elevated
//!   prompt for the same reason.
//!
//! The enrolling user's SID comes from the service's command line (admin-only
//! service configuration), never from a user-writable file.
//!
//! The SDDL builders are plain functions, unit-tested on every platform.

/// True for a well-formed SID string (`S-1-...`, digits and dashes only).
/// Anything else is refused, so the value can never add an access rule of
/// its own to a security descriptor.
pub fn is_valid_sid(sid: &str) -> bool {
    let Some(rest) = sid.strip_prefix("S-1-") else {
        return false;
    };
    !rest.is_empty()
        && rest
            .split('-')
            .all(|part| !part.is_empty() && part.bytes().all(|b| b.is_ascii_digit()))
}

/// The IPC pipe's security descriptor: protected DACL, full access for
/// SYSTEM and Administrators, read/write for the enrolling user. With
/// `owner_system` the pipe is explicitly owned by SYSTEM (the service), which
/// is what the CLI checks for. Without a valid user SID it fails closed to
/// SYSTEM and Administrators only.
pub fn pipe_sddl(user_sid: Option<&str>, owner_system: bool) -> String {
    let mut sddl = String::new();
    if owner_system {
        sddl.push_str("O:SY");
    }
    sddl.push_str("D:P(A;;GA;;;SY)(A;;GA;;;BA)");
    if let Some(sid) = user_sid.filter(|s| is_valid_sid(s)) {
        sddl.push_str(&format!("(A;;GRGW;;;{sid})"));
    }
    sddl
}

/// The directories under `%ProgramData%\Zecurity`, each with its own DACL.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum DataDir {
    /// `Zecurity` itself: holds client.conf.
    Root,
    /// `state`: the encrypted state and its key.
    State,
    /// `logs`: the daemon log.
    Logs,
}

/// FILE_GENERIC_READ | FILE_GENERIC_EXECUTE: list and traverse a directory.
const DIR_READ: &str = "0x1200a9";
/// DIR_READ | FILE_DELETE_CHILD: also delete the files in it (logout, and
/// login after a revocation, clear stale state from the CLI).
const DIR_READ_DELETE_CHILD: &str = "0x1200e9";

/// A data directory's DACL: protected (nothing inherited from
/// `C:\ProgramData`, which lets Users create folders), full control for
/// SYSTEM and Administrators on the directory and everything created in it.
/// The enrolled user gets read access (plus deleting files in `state`), and
/// never the right to add, rename or replace anything.
pub fn data_dir_sddl(dir: DataDir, user_sid: Option<&str>) -> String {
    let mut sddl = String::from("D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)");
    if let Some(sid) = user_sid.filter(|s| is_valid_sid(s)) {
        let user = match dir {
            DataDir::Root => format!("(A;;{DIR_READ};;;{sid})(A;OIIO;FR;;;{sid})"),
            DataDir::State => {
                format!("(A;;{DIR_READ_DELETE_CHILD};;;{sid})(A;OIIO;FR;;;{sid})")
            }
            DataDir::Logs => format!("(A;OICI;{DIR_READ};;;{sid})"),
        };
        sddl.push_str(&user);
    }
    sddl
}

#[cfg(windows)]
pub use imp::*;

#[cfg(windows)]
mod imp {
    use std::ffi::c_void;
    use std::os::windows::ffi::OsStrExt;
    use std::os::windows::io::RawHandle;
    use std::path::Path;

    use anyhow::{anyhow, bail, Context, Result};
    use windows_sys::Win32::Foundation::{LocalFree, ERROR_SUCCESS};
    use windows_sys::Win32::Security::Authorization::{
        ConvertSidToStringSidW, ConvertStringSecurityDescriptorToSecurityDescriptorW,
        GetNamedSecurityInfoW, GetSecurityInfo, SetNamedSecurityInfoW, SDDL_REVISION_1,
        SE_FILE_OBJECT, SE_KERNEL_OBJECT,
    };
    use windows_sys::Win32::Security::{
        GetSecurityDescriptorDacl, IsWellKnownSid, SetFileSecurityW, WinBuiltinAdministratorsSid,
        WinLocalSystemSid, ACL, DACL_SECURITY_INFORMATION, OWNER_SECURITY_INFORMATION,
        PROTECTED_DACL_SECURITY_INFORMATION, PSECURITY_DESCRIPTOR, PSID,
        UNPROTECTED_DACL_SECURITY_INFORMATION,
    };
    use windows_sys::Win32::Storage::FileSystem::FILE_ATTRIBUTE_REPARSE_POINT;

    use super::DataDir;

    /// A security descriptor allocated by Windows (LocalAlloc), freed on drop.
    pub struct OwnedDescriptor(pub PSECURITY_DESCRIPTOR);

    impl Drop for OwnedDescriptor {
        fn drop(&mut self) {
            if !self.0.is_null() {
                // SAFETY: allocated by a Windows API that documents LocalFree.
                unsafe { LocalFree(self.0 as _) };
            }
        }
    }

    /// Parses SDDL into a self-relative security descriptor.
    pub fn descriptor_from_sddl(sddl: &str) -> std::io::Result<OwnedDescriptor> {
        let wide: Vec<u16> = sddl.encode_utf16().chain(Some(0)).collect();
        let mut sd: PSECURITY_DESCRIPTOR = std::ptr::null_mut();
        // SAFETY: `wide` is NUL-terminated; `sd` receives a LocalAlloc'd buffer.
        let ok = unsafe {
            ConvertStringSecurityDescriptorToSecurityDescriptorW(
                wide.as_ptr(),
                SDDL_REVISION_1,
                &mut sd,
                std::ptr::null_mut(),
            )
        };
        if ok == 0 {
            return Err(std::io::Error::last_os_error());
        }
        Ok(OwnedDescriptor(sd))
    }

    fn wide_path(path: &Path) -> Vec<u16> {
        path.as_os_str().encode_wide().chain(Some(0)).collect()
    }

    fn sid_string(sid: PSID) -> String {
        let mut out: *mut u16 = std::ptr::null_mut();
        // SAFETY: `sid` is a valid SID from a security descriptor we hold.
        if unsafe { ConvertSidToStringSidW(sid, &mut out) } == 0 || out.is_null() {
            return "<unknown>".into();
        }
        // SAFETY: `out` is a NUL-terminated LocalAlloc'd string.
        let s = unsafe {
            let len = (0..).take_while(|&i| *out.add(i) != 0).count();
            String::from_utf16_lossy(std::slice::from_raw_parts(out, len))
        };
        // SAFETY: allocated by ConvertSidToStringSidW.
        unsafe { LocalFree(out as _) };
        s
    }

    fn is_system_or_admins(sid: PSID) -> bool {
        // SAFETY: `sid` is a valid SID.
        unsafe {
            IsWellKnownSid(sid, WinLocalSystemSid) != 0
                || IsWellKnownSid(sid, WinBuiltinAdministratorsSid) != 0
        }
    }

    /// Refuses a junction, symlink or other reparse point: the SYSTEM daemon
    /// must never act on a path that leads somewhere else.
    fn reject_reparse_point(path: &Path) -> Result<()> {
        use std::os::windows::fs::MetadataExt;
        let meta = std::fs::symlink_metadata(path)
            .with_context(|| format!("inspect {}", path.display()))?;
        if meta.file_attributes() & FILE_ATTRIBUTE_REPARSE_POINT != 0 {
            bail!(
                "{} is a junction or link; refusing to use it (an administrator should \
                 remove it and reinstall the client)",
                path.display()
            );
        }
        Ok(())
    }

    /// The owner of a file or directory must be SYSTEM or Administrators:
    /// nothing in the data directory is ever created by anyone else.
    fn check_path_owner(path: &Path) -> Result<()> {
        let wide = wide_path(path);
        let mut owner: PSID = std::ptr::null_mut();
        let mut sd: PSECURITY_DESCRIPTOR = std::ptr::null_mut();
        // SAFETY: out-pointers are valid; `owner` points into `sd`.
        let rc = unsafe {
            GetNamedSecurityInfoW(
                wide.as_ptr(),
                SE_FILE_OBJECT,
                OWNER_SECURITY_INFORMATION,
                &mut owner,
                std::ptr::null_mut(),
                std::ptr::null_mut(),
                std::ptr::null_mut(),
                &mut sd,
            )
        };
        if rc != ERROR_SUCCESS {
            bail!(
                "read the owner of {}: {}",
                path.display(),
                std::io::Error::from_raw_os_error(rc as i32)
            );
        }
        let sd = OwnedDescriptor(sd);
        let trusted = is_system_or_admins(owner);
        let owner_str = sid_string(owner);
        drop(sd);
        if !trusted {
            bail!(
                "{} is owned by {owner_str}, not SYSTEM or Administrators; refusing to use \
                 it (an administrator should remove it and reinstall the client)",
                path.display()
            );
        }
        Ok(())
    }

    /// Sets a data directory's protected DACL. SetFileSecurityW changes this
    /// one object only: it does not walk existing children, so it can never
    /// carry the user's ACEs onto anything linked in below it.
    fn set_dir_dacl(path: &Path, dir: DataDir, user_sid: Option<&str>) -> Result<()> {
        let sd = descriptor_from_sddl(&super::data_dir_sddl(dir, user_sid))
            .context("build the data directory security descriptor")?;
        let wide = wide_path(path);
        // SAFETY: `wide` is NUL-terminated; `sd` is a valid descriptor.
        let ok = unsafe {
            SetFileSecurityW(
                wide.as_ptr(),
                DACL_SECURITY_INFORMATION | PROTECTED_DACL_SECURITY_INFORMATION,
                sd.0,
            )
        };
        if ok == 0 {
            bail!(
                "restrict access to {}: {}",
                path.display(),
                std::io::Error::last_os_error()
            );
        }
        Ok(())
    }

    /// Replaces a file's DACL with an empty, unprotected one, so the file has
    /// exactly what its directory grants (SYSTEM and Administrators full
    /// control, the enrolled user read). Whatever explicit access the file
    /// carried before, such as write access for some other account, is
    /// gone: a trusted owner alone never makes a file trusted. Files have no
    /// children, so nothing propagates beyond `path`.
    fn reset_file_dacl(path: &Path) -> Result<()> {
        let sd = descriptor_from_sddl("D:AI").context("build an empty DACL")?;
        let mut present = 0;
        let mut defaulted = 0;
        let mut dacl: *mut ACL = std::ptr::null_mut();
        // SAFETY: `sd.0` is a valid descriptor; `dacl` points into it.
        if unsafe { GetSecurityDescriptorDacl(sd.0, &mut present, &mut dacl, &mut defaulted) } == 0
            || present == 0
        {
            bail!("empty security descriptor has no DACL");
        }
        let wide = wide_path(path);
        // SAFETY: `wide` is NUL-terminated; `dacl` stays valid while `sd` lives.
        let rc = unsafe {
            SetNamedSecurityInfoW(
                wide.as_ptr(),
                SE_FILE_OBJECT,
                DACL_SECURITY_INFORMATION | UNPROTECTED_DACL_SECURITY_INFORMATION,
                std::ptr::null_mut(),
                std::ptr::null_mut(),
                dacl,
                std::ptr::null(),
            )
        };
        if rc != ERROR_SUCCESS {
            bail!(
                "reset access to {}: {}",
                path.display(),
                std::io::Error::from_raw_os_error(rc as i32)
            );
        }
        Ok(())
    }

    /// Run by the daemon (LocalSystem) before it reads config or state:
    /// creates `%ProgramData%\Zecurity` and its `state` and `logs`
    /// directories if missing; refuses any of them, or any entry in them,
    /// that is a reparse point or not owned by SYSTEM/Administrators; sets
    /// each directory's protected DACL; and resets every file to the access
    /// its directory grants.
    pub fn secure_data_dir(user_sid: Option<&str>) -> Result<()> {
        let root = crate::config::program_data_dir();
        let dirs = [
            (root.clone(), DataDir::Root),
            (root.join("state"), DataDir::State),
            (root.join("logs"), DataDir::Logs),
        ];
        for (path, kind) in &dirs {
            std::fs::create_dir_all(path).with_context(|| format!("create {}", path.display()))?;
            reject_reparse_point(path)?;
            check_path_owner(path)?;
            set_dir_dacl(path, *kind, user_sid)?;
        }
        for (path, _) in &dirs {
            for entry in
                std::fs::read_dir(path).with_context(|| format!("list {}", path.display()))?
            {
                let entry = entry?;
                let path = entry.path();
                reject_reparse_point(&path)?;
                check_path_owner(&path)?;
                if entry.file_type()?.is_file() {
                    reset_file_dacl(&path)?;
                }
            }
        }
        Ok(())
    }

    /// CLI side: the pipe it connected to must be owned by SYSTEM (the
    /// service) or Administrators (an elevated console daemon). A pipe owned
    /// by anyone else is an impostor, and the CLI must not send it tokens or
    /// keys.
    pub fn verify_pipe_server(handle: RawHandle) -> Result<()> {
        let mut owner: PSID = std::ptr::null_mut();
        let mut sd: PSECURITY_DESCRIPTOR = std::ptr::null_mut();
        // SAFETY: `handle` is an open pipe handle; out-pointers are valid.
        let rc = unsafe {
            GetSecurityInfo(
                handle as _,
                SE_KERNEL_OBJECT,
                OWNER_SECURITY_INFORMATION,
                &mut owner,
                std::ptr::null_mut(),
                std::ptr::null_mut(),
                std::ptr::null_mut(),
                &mut sd,
            )
        };
        if rc != ERROR_SUCCESS {
            return Err(anyhow!(
                "read the owner of the daemon pipe: {}",
                std::io::Error::from_raw_os_error(rc as i32)
            ));
        }
        let sd = OwnedDescriptor(sd);
        let trusted = is_system_or_admins(owner);
        let owner_str = sid_string(owner);
        drop(sd);
        if !trusted {
            bail!(
                "the daemon pipe is owned by {owner_str}, not SYSTEM: refusing to talk to it \
                 (is the zecurity-client service running?)"
            );
        }
        Ok(())
    }

    /// The SID string of the user this process runs as (tests only).
    #[cfg(test)]
    pub fn current_user_sid() -> Result<String> {
        use windows_sys::Win32::Foundation::{CloseHandle, HANDLE};
        use windows_sys::Win32::Security::{
            GetTokenInformation, TokenUser, TOKEN_QUERY, TOKEN_USER,
        };
        use windows_sys::Win32::System::Threading::{GetCurrentProcess, OpenProcessToken};

        let mut token: HANDLE = std::ptr::null_mut();
        // SAFETY: the pseudo-handle and out-pointer are valid.
        if unsafe { OpenProcessToken(GetCurrentProcess(), TOKEN_QUERY, &mut token) } == 0 {
            bail!("OpenProcessToken: {}", std::io::Error::last_os_error());
        }
        let mut buf = vec![0u8; 256];
        let mut len = 0u32;
        // SAFETY: `buf` is writable for `buf.len()` bytes.
        let ok = unsafe {
            GetTokenInformation(
                token,
                TokenUser,
                buf.as_mut_ptr() as *mut c_void,
                buf.len() as u32,
                &mut len,
            )
        };
        // SAFETY: `token` was opened above.
        unsafe { CloseHandle(token) };
        if ok == 0 {
            bail!("GetTokenInformation: {}", std::io::Error::last_os_error());
        }
        // SAFETY: on success `buf` starts with a TOKEN_USER.
        let user = unsafe { &*(buf.as_ptr() as *const TOKEN_USER) };
        Ok(sid_string(user.User.Sid))
    }

    /// Pointer form of a descriptor for `SECURITY_ATTRIBUTES`.
    pub fn as_ptr(sd: &OwnedDescriptor) -> *mut c_void {
        sd.0
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sid_validation_accepts_only_plain_sid_strings() {
        assert!(is_valid_sid(
            "S-1-5-21-1004336348-1177238915-682003330-1001"
        ));
        assert!(is_valid_sid("S-1-5-18"));
        for bad in [
            "",
            "S-1-",
            "S-1-5-",
            "S-1-5--18",
            "s-1-5-18",
            "S-1-5-18)(A;;GA;;;WD",
            "S-1-5-18 ",
            "WD",
        ] {
            assert!(!is_valid_sid(bad), "{bad:?} must be rejected");
        }
    }

    #[test]
    fn pipe_sddl_admits_system_admins_and_the_enrolling_user_only() {
        assert_eq!(
            pipe_sddl(Some("S-1-5-21-1-2-3-1001"), true),
            "O:SYD:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GRGW;;;S-1-5-21-1-2-3-1001)"
        );
        assert_eq!(
            pipe_sddl(Some("S-1-5-21-1-2-3-1001"), false),
            "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GRGW;;;S-1-5-21-1-2-3-1001)"
        );
    }

    #[test]
    fn sddl_fails_closed_without_a_valid_sid() {
        assert_eq!(pipe_sddl(None, true), "O:SYD:P(A;;GA;;;SY)(A;;GA;;;BA)");
        // An injection attempt adds nothing: Everyone (WD) is never granted.
        assert_eq!(
            pipe_sddl(Some("S-1-5-18)(A;;GA;;;WD"), false),
            "D:P(A;;GA;;;SY)(A;;GA;;;BA)"
        );
        for dir in [DataDir::Root, DataDir::State, DataDir::Logs] {
            assert_eq!(
                data_dir_sddl(dir, Some("S-1-1-0)(A;OICI;FA;;;WD")),
                "D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
            );
        }
    }

    /// Windows' own SDDL parser accepts every descriptor the client builds.
    #[cfg(windows)]
    #[test]
    fn every_descriptor_parses_on_windows() {
        let u = Some("S-1-5-21-1-2-3-1001");
        let mut all = vec![
            pipe_sddl(u, true),
            pipe_sddl(u, false),
            pipe_sddl(None, true),
        ];
        for dir in [DataDir::Root, DataDir::State, DataDir::Logs] {
            all.push(data_dir_sddl(dir, u));
            all.push(data_dir_sddl(dir, None));
        }
        for sddl in all {
            descriptor_from_sddl(&sddl).unwrap_or_else(|e| panic!("{sddl}: {e}"));
        }
    }

    #[test]
    fn the_enrolled_user_can_read_but_never_add_or_replace_anything() {
        let u = "S-1-5-21-1-2-3-1001";
        assert_eq!(
            data_dir_sddl(DataDir::Root, Some(u)),
            format!("D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;;0x1200a9;;;{u})(A;OIIO;FR;;;{u})")
        );
        assert_eq!(
            data_dir_sddl(DataDir::State, Some(u)),
            format!("D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;;0x1200e9;;;{u})(A;OIIO;FR;;;{u})")
        );
        assert_eq!(
            data_dir_sddl(DataDir::Logs, Some(u)),
            format!("D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;{u})")
        );
        // The user's directory rights: read/list/traverse, plus delete-child
        // on state. Never FILE_ADD_FILE (0x2), FILE_ADD_SUBDIRECTORY (0x4),
        // DELETE (0x10000), WRITE_DAC (0x40000) or WRITE_OWNER (0x80000).
        for mask in [0x1200a9u32, 0x1200e9] {
            assert_eq!(mask & (0x2 | 0x4 | 0x10000 | 0x40000 | 0x80000), 0);
        }
        assert_eq!(super::DIR_READ, "0x1200a9");
        assert_eq!(super::DIR_READ_DELETE_CHILD, "0x1200e9");
    }
}
