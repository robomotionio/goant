function check(a,b){if(a!==b)throw a+" !== "+b;} var e=new Error('msg');check(e.message,'msg');check(e.name,'Error');check(e instanceof Error,true);console.log('PASS');
