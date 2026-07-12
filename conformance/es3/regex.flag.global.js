function check(a,b){if(a!==b)throw a+" !== "+b;} check(/x/g.global,true);check(/x/.global,false);var r=/a/g;check('aaa'.replace(r,'b'),'bbb');console.log('PASS');
