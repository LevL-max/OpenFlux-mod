"""Protect recovery signing material with Windows DPAPI for the current user."""
import ctypes
from ctypes import wintypes
class Blob(ctypes.Structure):
 _fields_=[('size',wintypes.DWORD),('data',ctypes.POINTER(ctypes.c_byte))]
def transform(data,decrypt=False):
 buf=ctypes.create_string_buffer(data);src=Blob(len(data),ctypes.cast(buf,ctypes.POINTER(ctypes.c_byte)));dst=Blob()
 api=ctypes.windll.crypt32
 if decrypt:ok=api.CryptUnprotectData(ctypes.byref(src),None,None,None,None,1,ctypes.byref(dst))
 else:ok=api.CryptProtectData(ctypes.byref(src),'OpenFlux Recovery',None,None,None,1,ctypes.byref(dst))
 if not ok:raise ctypes.WinError()
 try:return ctypes.string_at(dst.data,dst.size)
 finally:ctypes.windll.kernel32.LocalFree(dst.data)
def protect(data):return transform(data)
def unprotect(data):return transform(data,True)
