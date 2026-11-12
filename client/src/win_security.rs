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
//! - the daemon refuses a data directory, or a file in it, owned by an
//!   untrusted account (no pre-created `C:\ProgramData\Zecurity`), and sets a
//!   protected DACL on it before reading anything.
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

/// The data directory's DACL: protected (nothing inherited from
/// `C:\ProgramData`, which grants Users read access), full control for
/// SYSTEM, Administrators and the enrolling user, inherited by every file
/// and subdirectory.
pub fn data_dir_sddl(user_sid: Option<&str>) -> String {
    let mut sddl = String::from("D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)");
    if let Some(sid) = user_sid.filter(|s| is_valid_sid(s)) {
        sddl.push_str(&format!("(A;OICI;FA;;;{sid})"));
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
        GetSecurityDescriptorDacl, IsWellKnownSid, WinBuiltinAdministratorsSid,
        WinLocalSystemSid, ACL, DACL_SECURITY_INFORMATION, OWNER_SECURITY_INFORMATION,
        PROTECTED_DACL_SECURITY_INFORMATION, PSECURITY_DESCRIPTOR, PSID,
    };

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

    /// The owner of a file or directory, checked against SYSTEM,
    /// Administrators and (if given) the enrolling user.
    fn check_path_owner(path: &Path, user_sid: Option<&str>) -> Result<()> {
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
        let owner_str = sid_string(owner);
        let trusted =
            is_system_or_admins(owner) || user_sid.is_some_and(|u| u == owner_str);
        drop(sd);
        if !trusted {
            bail!(
                "{} is owned by {owner_str}, which is not SYSTEM, Administrators or the \
                 enrolled user; refusing to use it (an administrator should remove it and \
                 reinstall the client)",
                path.display()
            );
        }
        Ok(())
    }

    /// Sets the protected data-directory DACL on `path` (inherited by its
    /// contents).
    fn set_data_dir_dacl(path: &Path, user_sid: Option<&str>) -> Result<()> {
        let sd = descriptor_from_sddl(&super::data_dir_sddl(user_sid))
            .context("build the data directory security descriptor")?;
        let mut present = 0;
        let mut defaulted = 0;
        let mut dacl: *mut ACL = std::ptr::null_mut();
        // SAFETY: `sd.0` is a valid descriptor; `dacl` points into it.
        if unsafe { GetSecurityDescriptorDacl(sd.0, &mut present, &mut dacl, &mut defaulted) }
            == 0
            || present == 0
        {
            bail!("data directory security descriptor has no DACL");
        }
        let wide = wide_path(path);
        // SAFETY: `wide` is NUL-terminated; `dacl` stays valid while `sd` lives.
        let rc = unsafe {
            SetNamedSecurityInfoW(
                wide.as_ptr(),
                SE_FILE_OBJECT,
                DACL_SECURITY_INFORMATION | PROTECTED_DACL_SECURITY_INFORMATION,
                std::ptr::null_mut(),
                std::ptr::null_mut(),
                dacl,
                std::ptr::null(),
            )
        };
        if rc != ERROR_SUCCESS {
            bail!(
                "restrict access to {}: {}",
                path.display(),
                std::io::Error::from_raw_os_error(rc as i32)
            );
        }
        Ok(())
    }

    /// Run by the daemon (LocalSystem) before it reads config or state:
    /// creates `%ProgramData%\Zecurity` and its `state` and `logs`
    /// directories if missing, refuses any of them (or any file in them)
    /// owned by an untrusted account, and sets the protected DACL.
    pub fn secure_data_dir(user_sid: Option<&str>) -> Result<()> {
        let root = crate::config::program_data_dir();
        let dirs = [root.clone(), root.join("state"), root.join("logs")];
        for dir in &dirs {
            std::fs::create_dir_all(dir)
                .with_context(|| format!("create {}", dir.display()))?;
            check_path_owner(dir, user_sid)?;
            set_data_dir_dacl(dir, user_sid)?;
        }
        for dir in &dirs {
            for entry in std::fs::read_dir(dir)
                .with_context(|| format!("list {}", dir.display()))?
            {
                let entry = entry?;
                if entry.file_type()?.is_file() {
                    check_path_owner(&entry.path(), user_sid)?;
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
        use windows_sys::Win32::Security::{GetTokenInformation, TokenUser, TOKEN_QUERY, TOKEN_USER};
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
        assert!(is_valid_sid("S-1-5-21-1004336348-1177238915-682003330-1001"));
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
        assert_eq!(
            data_dir_sddl(Some("S-1-1-0)(A;OICI;FA;;;WD")),
            "D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
        );
    }

    #[test]
    fn data_dir_sddl_is_protected_and_inherited() {
        assert_eq!(
            data_dir_sddl(Some("S-1-5-21-1-2-3-1001")),
            "D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;S-1-5-21-1-2-3-1001)"
        );
    }
}
