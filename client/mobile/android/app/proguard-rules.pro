# JNI: libzecurity_ffi.so looks these up by name (ZecurityNative.startDaemon
# is native; ZecurityNative.establish is called from the daemon). Only native
# code references establish, so without this R8 removes or renames it.
-keep class io.zecurity.client.ZecurityNative { *; }
-keepclasseswithmembernames class * { native <methods>; }
